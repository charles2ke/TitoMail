package imapd

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/charles2ke/TitoMail/internal/store"
)

type updateKind uint8

const (
	updAdded updateKind = iota + 1
	updRemoved
	updFlags
)

// update is a committed store change relevant to a mailbox. It is shared
// (read-only) between all views of that mailbox.
type update struct {
	kind  updateKind
	uids  []uint32 // ascending
	flags map[uint32][]imap.Flag
}

// origin describes how a change relates to the view it is queued for.
type origin uint8

const (
	originOther       origin = iota
	originOwnReported        // caused by this view; FETCH responses already sent
	originOwnSilent          // caused by this view with STORE .SILENT
)

type queuedUpdate struct {
	u      *update
	origin origin
}

// sourceToken is passed as store.Change.Source by store writes a view
// performs, so the listener can recognize the view's own changes.
type sourceToken struct {
	view   *mailboxView
	silent bool
}

// mailboxView is a session's view of its selected mailbox.
//
// uids is the message list as known by the client: it only changes when
// queued updates are flushed to the client (flush), which keeps sequence
// numbers consistent with what the client has been told.
type mailboxView struct {
	srv         *Server
	id          int64
	userID      int64
	uidValidity uint32
	readOnly    bool
	notify      chan struct{}
	srcReported *sourceToken
	srcSilent   *sourceToken

	// Guarded by the owning session's mutex.
	uids      []uint32
	searchRes []uint32 // SEARCHRES ($) result, ascending UIDs
	gone      bool     // mailbox was deleted (or replaced)

	qmu     sync.Mutex
	queue   []queuedUpdate
	recheck bool // mailboxes changed; verify the mailbox still exists
}

func newMailboxView(srv *Server, mb *store.Mailbox, uidValidity uint32, uids []uint32, readOnly bool) *mailboxView {
	v := &mailboxView{
		srv:         srv,
		id:          mb.ID,
		userID:      mb.UserID,
		uidValidity: uidValidity,
		readOnly:    readOnly,
		notify:      make(chan struct{}, 1),
		uids:        uids,
	}
	v.srcReported = &sourceToken{view: v}
	v.srcSilent = &sourceToken{view: v, silent: true}
	return v
}

func (v *mailboxView) push(q queuedUpdate) {
	v.qmu.Lock()
	v.queue = append(v.queue, q)
	v.qmu.Unlock()
	v.wake()
}

func (v *mailboxView) markRecheck() {
	v.qmu.Lock()
	v.recheck = true
	v.qmu.Unlock()
	v.wake()
}

func (v *mailboxView) wake() {
	select {
	case v.notify <- struct{}{}:
	default:
	}
}

// exists reports whether the mailbox the view was created for still exists.
func (v *mailboxView) exists(ctx context.Context) (bool, error) {
	mb, err := v.srv.st.GetMailbox(ctx, v.userID, v.id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return mb.UIDValidity == v.uidValidity, nil
}

func (v *mailboxView) markGone() {
	if v.gone {
		return
	}
	v.gone = true
	v.srv.unregister(v)
}

// check verifies, before a command operates on the mailbox, that it still
// exists if mailboxes changed since the last flush. The recheck flag is
// left set so that the next flush also sees it.
func (v *mailboxView) check(ctx context.Context) {
	if v.gone {
		return
	}
	v.qmu.Lock()
	recheck := v.recheck
	v.qmu.Unlock()
	if !recheck {
		return
	}
	if ok, err := v.exists(ctx); err == nil && !ok {
		v.markGone()
	}
}

// flush sends queued updates to the client: EXISTS for new messages,
// EXPUNGE for removed ones (only if allowExpunge) and FETCH FLAGS for flag
// changes not caused by this session's own (reported) commands.
func (v *mailboxView) flush(ctx context.Context, w *imapserver.UpdateWriter, allowExpunge bool) error {
	v.qmu.Lock()
	recheck := v.recheck
	v.recheck = false
	n := len(v.queue)
	if !allowExpunge {
		for i, q := range v.queue {
			if q.u.kind == updRemoved {
				n = i
				break
			}
		}
	}
	batch := v.queue[:n]
	if rest := v.queue[n:]; len(rest) > 0 {
		v.queue = append([]queuedUpdate(nil), rest...)
	} else {
		v.queue = nil
	}
	v.qmu.Unlock()

	// Checked after taking the batch: if the mailbox was replaced by a new
	// one with the same ID, any update of the new mailbox in the batch was
	// committed before this check and is therefore ignored.
	if recheck && !v.gone {
		ok, err := v.exists(ctx)
		switch {
		case err != nil:
			v.qmu.Lock()
			v.recheck = true
			v.qmu.Unlock()
		case !ok:
			v.markGone()
		}
	}

	pendingExists := false
	var (
		latest map[uint32][]imap.Flag
		report map[uint32]bool
	)
	for _, q := range batch {
		switch q.u.kind {
		case updAdded:
			if v.gone {
				continue
			}
			for _, uid := range q.u.uids {
				if len(v.uids) == 0 || uid > v.uids[len(v.uids)-1] {
					v.uids = append(v.uids, uid)
					pendingExists = true
				}
			}
		case updRemoved:
			idx := v.indicesOf(q.u.uids)
			if len(idx) == 0 {
				continue
			}
			if pendingExists {
				if err := w.WriteNumMessages(uint32(len(v.uids))); err != nil {
					return err
				}
				pendingExists = false
			}
			// Descending order keeps the remaining sequence numbers valid.
			for i := len(idx) - 1; i >= 0; i-- {
				if err := w.WriteExpunge(uint32(idx[i] + 1)); err != nil {
					return err
				}
			}
			v.removeIndices(idx)
			for _, uid := range q.u.uids {
				delete(latest, uid)
				delete(report, uid)
			}
		case updFlags:
			for _, uid := range q.u.uids {
				fl, ok := q.u.flags[uid]
				if !ok {
					continue
				}
				if _, ok := v.indexOf(uid); !ok {
					continue
				}
				if latest == nil {
					latest = make(map[uint32][]imap.Flag)
					report = make(map[uint32]bool)
				}
				latest[uid] = fl
				switch q.origin {
				case originOther:
					report[uid] = true
				case originOwnReported:
					report[uid] = false
				}
			}
		}
	}
	if pendingExists {
		if err := w.WriteNumMessages(uint32(len(v.uids))); err != nil {
			return err
		}
	}
	if len(report) == 0 {
		return nil
	}
	uids := make([]uint32, 0, len(report))
	for uid, ok := range report {
		if ok {
			uids = append(uids, uid)
		}
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	for _, uid := range uids {
		i, ok := v.indexOf(uid)
		if !ok {
			continue
		}
		if err := w.WriteMessageFlags(uint32(i+1), imap.UID(uid), latest[uid]); err != nil {
			return err
		}
	}
	return nil
}

func (v *mailboxView) indexOf(uid uint32) (int, bool) {
	i := sort.Search(len(v.uids), func(i int) bool { return v.uids[i] >= uid })
	return i, i < len(v.uids) && v.uids[i] == uid
}

// indicesOf returns the ascending indices of the given (ascending) UIDs
// that are part of the view.
func (v *mailboxView) indicesOf(uids []uint32) []int {
	var idx []int
	for _, uid := range uids {
		if i, ok := v.indexOf(uid); ok {
			idx = append(idx, i)
		}
	}
	return idx
}

// removeIndices removes the given ascending indices from v.uids.
func (v *mailboxView) removeIndices(idx []int) {
	out := v.uids[:idx[0]]
	next := 0
	for i := idx[0]; i < len(v.uids); i++ {
		if next < len(idx) && idx[next] == i {
			next++
			continue
		}
		out = append(out, v.uids[i])
	}
	v.uids = out
}

// resolve returns the ascending, de-duplicated indices into v.uids of the
// messages referenced by numSet. Numbers not matching any message are
// ignored.
func (v *mailboxView) resolve(numSet imap.NumSet) []int {
	n := len(v.uids)
	if n == 0 || numSet == nil {
		return nil
	}
	if imap.IsSearchRes(numSet) {
		var idx []int
		j := 0
		for i, uid := range v.uids {
			for j < len(v.searchRes) && v.searchRes[j] < uid {
				j++
			}
			if j < len(v.searchRes) && v.searchRes[j] == uid {
				idx = append(idx, i)
			}
		}
		return idx
	}
	type span struct{ lo, hi int } // [lo, hi)
	var spans []span
	switch set := numSet.(type) {
	case imap.SeqSet:
		for _, r := range set {
			start, stop := r.Start, r.Stop
			if start == 0 {
				start = uint32(n)
			}
			if stop == 0 {
				stop = uint32(n)
			}
			if start > stop {
				start, stop = stop, start
			}
			if start > uint32(n) {
				continue
			}
			if stop > uint32(n) {
				stop = uint32(n)
			}
			spans = append(spans, span{int(start) - 1, int(stop)})
		}
	case imap.UIDSet:
		maxUID := v.uids[n-1]
		for _, r := range set {
			start, stop := uint32(r.Start), uint32(r.Stop)
			if start == 0 {
				start = maxUID
			}
			if stop == 0 {
				stop = maxUID
			}
			if start > stop {
				start, stop = stop, start
			}
			lo := sort.Search(n, func(i int) bool { return v.uids[i] >= start })
			hi := sort.Search(n, func(i int) bool { return v.uids[i] > stop })
			if lo < hi {
				spans = append(spans, span{lo, hi})
			}
		}
	default:
		return nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })
	var idx []int
	next := 0
	for _, sp := range spans {
		lo := sp.lo
		if lo < next {
			lo = next
		}
		for i := lo; i < sp.hi; i++ {
			idx = append(idx, i)
		}
		if sp.hi > next {
			next = sp.hi
		}
	}
	return idx
}

// contains reports whether the message at index i is referenced by numSet.
func (v *mailboxView) contains(numSet imap.NumSet, i int) bool {
	n := len(v.uids)
	uid := v.uids[i]
	if imap.IsSearchRes(numSet) {
		j := sort.Search(len(v.searchRes), func(k int) bool { return v.searchRes[k] >= uid })
		return j < len(v.searchRes) && v.searchRes[j] == uid
	}
	switch set := numSet.(type) {
	case imap.SeqSet:
		seq := uint32(i + 1)
		for _, r := range set {
			start, stop := r.Start, r.Stop
			if start == 0 {
				start = uint32(n)
			}
			if stop == 0 {
				stop = uint32(n)
			}
			if start > stop {
				start, stop = stop, start
			}
			if seq >= start && seq <= stop {
				return true
			}
		}
	case imap.UIDSet:
		maxUID := v.uids[n-1]
		for _, r := range set {
			start, stop := uint32(r.Start), uint32(r.Stop)
			if start == 0 {
				start = maxUID
			}
			if stop == 0 {
				stop = maxUID
			}
			if start > stop {
				start, stop = stop, start
			}
			if uid >= start && uid <= stop {
				return true
			}
		}
	}
	return false
}

func (v *mailboxView) uidsAt(idx []int) []uint32 {
	out := make([]uint32, len(idx))
	for i, j := range idx {
		out[i] = v.uids[j]
	}
	return out
}

func toIMAPFlags(fl []string) []imap.Flag {
	out := make([]imap.Flag, len(fl))
	for i, f := range fl {
		out[i] = imap.Flag(f)
	}
	return out
}

func fromIMAPFlags(fl []imap.Flag) []string {
	out := make([]string, len(fl))
	for i, f := range fl {
		out[i] = string(f)
	}
	return out
}

// uidSet builds a compact UID set from ascending UIDs, preserving order.
func uidSet(uids []uint32) imap.UIDSet {
	set := imap.UIDSet{}
	for i := 0; i < len(uids); {
		j := i
		for j+1 < len(uids) && uids[j+1] == uids[j]+1 {
			j++
		}
		set = append(set, imap.UIDRange{Start: imap.UID(uids[i]), Stop: imap.UID(uids[j])})
		i = j + 1
	}
	return set
}

// seqSet builds a compact sequence set from ascending numbers.
func seqSet(nums []uint32) imap.SeqSet {
	set := imap.SeqSet{}
	for i := 0; i < len(nums); {
		j := i
		for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
			j++
		}
		set = append(set, imap.SeqRange{Start: nums[i], Stop: nums[j]})
		i = j + 1
	}
	return set
}
