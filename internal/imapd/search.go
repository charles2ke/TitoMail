package imapd

import (
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

// searchBatch is the number of messages loaded from the store at once.
const searchBatch = 500

func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if usesModSeq(criteria) {
		return nil, errNoCondStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.selectedView()
	if err != nil {
		return nil, err
	}
	var matched []int // indices into v.uids
	if !v.gone {
		matched, err = s.search(v, criteria)
		if err != nil {
			if options.ReturnSave {
				v.searchRes = nil
			}
			return nil, err
		}
	}

	nums := make([]uint32, len(matched))
	for i, j := range matched {
		if kind == imapserver.NumKindUID {
			nums[i] = v.uids[j]
		} else {
			nums[i] = uint32(j + 1)
		}
	}
	data := &imap.SearchData{Count: uint32(len(nums))}
	if kind == imapserver.NumKindUID {
		data.All = uidSet(nums)
	} else {
		data.All = seqSet(nums)
	}
	if len(nums) > 0 {
		data.Min, data.Max = nums[0], nums[len(nums)-1]
	}

	if options.ReturnSave {
		// RFC 5182: with MIN and/or MAX but neither ALL nor COUNT, only
		// those results are saved.
		var save []uint32
		if (options.ReturnMin || options.ReturnMax) && !options.ReturnAll && !options.ReturnCount {
			if len(matched) > 0 {
				first, last := v.uids[matched[0]], v.uids[matched[len(matched)-1]]
				if options.ReturnMin {
					save = append(save, first)
				}
				if options.ReturnMax && (!options.ReturnMin || last != first) {
					save = append(save, last)
				}
			}
		} else {
			save = make([]uint32, len(matched))
			for i, j := range matched {
				save[i] = v.uids[j]
			}
		}
		v.searchRes = save
	}
	return data, nil
}

func usesModSeq(c *imap.SearchCriteria) bool {
	if c.ModSeq != nil {
		return true
	}
	for i := range c.Not {
		if usesModSeq(&c.Not[i]) {
			return true
		}
	}
	for i := range c.Or {
		if usesModSeq(&c.Or[i][0]) || usesModSeq(&c.Or[i][1]) {
			return true
		}
	}
	return false
}

// search returns the ascending indices into v.uids of the messages
// matching criteria.
func (s *session) search(v *mailboxView, criteria *imap.SearchCriteria) ([]int, error) {
	if len(v.uids) == 0 {
		return nil, nil
	}
	// Narrow the candidates using the top-level sequence and UID sets.
	c := *criteria
	var candidates []int
	switch {
	case len(c.SeqNum) > 0:
		candidates = v.resolve(c.SeqNum[0])
		c.SeqNum = c.SeqNum[1:]
	case len(c.UID) > 0:
		candidates = v.resolve(c.UID[0])
		c.UID = c.UID[1:]
	default:
		candidates = make([]int, len(v.uids))
		for i := range candidates {
			candidates[i] = i
		}
	}

	var matched []int
	if !needsMessage(&c) {
		for _, i := range candidates {
			if ok, _ := (&searchMsg{s: s, v: v, idx: i}).match(&c); ok {
				matched = append(matched, i)
			}
		}
		return matched, nil
	}

	for start := 0; start < len(candidates); start += searchBatch {
		batch := candidates[start:min(start+searchBatch, len(candidates))]
		msgs, err := s.srv.st.MessagesByUID(s.ctx, v.id, v.uidsAt(batch))
		if err != nil {
			return nil, s.storeErr(err)
		}
		k := 0
		for _, m := range msgs {
			if m.UserID != v.userID {
				continue
			}
			for k < len(batch) && v.uids[batch[k]] < m.UID {
				k++
			}
			if k == len(batch) || v.uids[batch[k]] != m.UID {
				continue
			}
			ok, err := (&searchMsg{s: s, v: v, idx: batch[k], m: m}).match(&c)
			if err != nil {
				return nil, err
			}
			if ok {
				matched = append(matched, batch[k])
			}
		}
	}
	return matched, nil
}

// needsMessage reports whether evaluating c requires the message data (as
// opposed to only sequence numbers and UIDs).
func needsMessage(c *imap.SearchCriteria) bool {
	if !c.Since.IsZero() || !c.Before.IsZero() || !c.SentSince.IsZero() || !c.SentBefore.IsZero() ||
		len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 ||
		len(c.Flag) > 0 || len(c.NotFlag) > 0 || c.Larger > 0 || c.Smaller > 0 {
		return true
	}
	for i := range c.Not {
		if needsMessage(&c.Not[i]) {
			return true
		}
	}
	for i := range c.Or {
		if needsMessage(&c.Or[i][0]) || needsMessage(&c.Or[i][1]) {
			return true
		}
	}
	return false
}

// searchMsg evaluates search criteria against one message, loading and
// parsing the raw message only when needed.
type searchMsg struct {
	s   *session
	v   *mailboxView
	idx int
	m   *store.Message // nil if no message data is needed

	flags map[string]bool

	raw       []byte
	rawLoaded bool
	header    *mail.Header
	body      *string
	text      *string
}

func (sm *searchMsg) match(c *imap.SearchCriteria) (bool, error) {
	for _, set := range c.SeqNum {
		if !sm.v.contains(set, sm.idx) {
			return false, nil
		}
	}
	for _, set := range c.UID {
		if !sm.v.contains(set, sm.idx) {
			return false, nil
		}
	}
	m := sm.m
	if m == nil {
		return matchNested(sm, c)
	}

	if len(c.Flag) > 0 || len(c.NotFlag) > 0 {
		if sm.flags == nil {
			sm.flags = make(map[string]bool)
			for _, f := range m.Flags() {
				sm.flags[strings.ToLower(f)] = true
			}
		}
		for _, f := range c.Flag {
			if !sm.flags[strings.ToLower(string(f))] {
				return false, nil
			}
		}
		for _, f := range c.NotFlag {
			if sm.flags[strings.ToLower(string(f))] {
				return false, nil
			}
		}
	}
	if c.Larger > 0 && m.Size <= c.Larger {
		return false, nil
	}
	if c.Smaller > 0 && m.Size >= c.Smaller {
		return false, nil
	}
	if !c.Since.IsZero() || !c.Before.IsZero() {
		d := day(m.ReceivedAt.UTC())
		if !c.Since.IsZero() && d.Before(day(c.Since)) {
			return false, nil
		}
		if !c.Before.IsZero() && !d.Before(day(c.Before)) {
			return false, nil
		}
	}
	if !c.SentSince.IsZero() {
		ok, err := sm.sentOnOrAfter(day(c.SentSince))
		if err != nil || !ok {
			return false, err
		}
	}
	if !c.SentBefore.IsZero() {
		ok, err := sm.sentOnOrAfter(day(c.SentBefore))
		if err != nil || ok {
			return false, err
		}
	}
	if ok, err := matchNested(sm, c); err != nil || !ok {
		return false, err
	}
	for _, f := range c.Header {
		ok, err := sm.matchHeader(f.Key, f.Value)
		if err != nil || !ok {
			return false, err
		}
	}
	for _, needle := range c.Body {
		body, err := sm.bodyText()
		if err != nil || !containsFold(body, needle) {
			return false, err
		}
	}
	for _, needle := range c.Text {
		text, err := sm.fullText()
		if err != nil || !containsFold(text, needle) {
			return false, err
		}
	}
	return true, nil
}

func matchNested(sm *searchMsg, c *imap.SearchCriteria) (bool, error) {
	for i := range c.Not {
		ok, err := sm.match(&c.Not[i])
		if err != nil || ok {
			return false, err
		}
	}
	for i := range c.Or {
		ok, err := sm.match(&c.Or[i][0])
		if err != nil {
			return false, err
		}
		if !ok {
			ok, err = sm.match(&c.Or[i][1])
			if err != nil || !ok {
				return false, err
			}
		}
	}
	return true, nil
}

// day returns the date of t (in t's location) as midnight UTC.
func day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// sentOnOrAfter reports whether the message's sent date (the date of its
// Date header field, in the field's time zone) is on or after d.
func (sm *searchMsg) sentOnOrAfter(d time.Time) (bool, error) {
	// The stored Date is an instant; time zone offsets range from -12:00 to
	// +14:00, which decides most cases without parsing the message.
	if t := sm.m.Date; !t.IsZero() {
		if !t.Before(d.Add(12 * time.Hour)) {
			return true, nil
		}
		if t.Before(d.Add(-14 * time.Hour)) {
			return false, nil
		}
	}
	h, err := sm.loadHeader()
	if err != nil {
		return false, err
	}
	t, err := h.Date()
	if err != nil || t.IsZero() {
		t = sm.m.Date
		if t.IsZero() {
			t = sm.m.ReceivedAt.UTC()
		}
	}
	return !day(t).Before(d), nil
}

func (sm *searchMsg) loadRaw() ([]byte, error) {
	if !sm.rawLoaded {
		raw, err := sm.s.srv.st.ReadBlob(sm.s.ctx, sm.m.BlobID)
		if err != nil {
			return nil, sm.s.internalErr(err)
		}
		sm.raw, sm.rawLoaded = raw, true
	}
	return sm.raw, nil
}

func (sm *searchMsg) loadHeader() (*mail.Header, error) {
	if sm.header == nil {
		raw, err := sm.loadRaw()
		if err != nil {
			return nil, err
		}
		h, _ := splitMessage(raw)
		sm.header = &mail.Header{Header: gomessage.Header{Header: h}}
	}
	return sm.header, nil
}

// matchHeader implements HEADER (and FROM, TO, CC, BCC, SUBJECT).
func (sm *searchMsg) matchHeader(key, value string) (bool, error) {
	if value != "" {
		m := sm.m
		switch strings.ToLower(key) {
		case "subject":
			return containsFold(m.Subject, value), nil
		case "from":
			return containsFold(formatAddresses([]email.Address{m.From}), value), nil
		case "to":
			return containsFold(formatAddresses(m.To), value), nil
		case "cc":
			return containsFold(formatAddresses(m.Cc), value), nil
		case "bcc":
			return containsFold(formatAddresses(m.Bcc), value), nil
		}
	}
	h, err := sm.loadHeader()
	if err != nil {
		return false, err
	}
	for fields := h.FieldsByKey(key); fields.Next(); {
		if value == "" {
			return true, nil
		}
		text, err := fields.Text()
		if err != nil {
			text = fields.Value()
		}
		if containsFold(text, value) || containsFold(fields.Value(), value) {
			return true, nil
		}
	}
	return false, nil
}

func (sm *searchMsg) bodyText() (string, error) {
	if sm.body == nil {
		raw, err := sm.loadRaw()
		if err != nil {
			return "", err
		}
		p := email.Parse(raw)
		body := p.Text
		if p.HTML != "" {
			body += "\n" + email.HTMLToText(p.HTML)
		}
		sm.body = &body
	}
	return *sm.body, nil
}

// fullText returns the decoded header fields followed by the body text.
func (sm *searchMsg) fullText() (string, error) {
	if sm.text == nil {
		h, err := sm.loadHeader()
		if err != nil {
			return "", err
		}
		body, err := sm.bodyText()
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for fields := h.Fields(); fields.Next(); {
			text, err := fields.Text()
			if err != nil {
				text = fields.Value()
			}
			b.WriteString(fields.Key())
			b.WriteString(": ")
			b.WriteString(text)
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(body)
		text := b.String()
		sm.text = &text
	}
	return *sm.text, nil
}

func formatAddresses(list []email.Address) string {
	var b strings.Builder
	for _, a := range list {
		if a.Name == "" && a.Address == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		if a.Name != "" {
			b.WriteString(a.Name)
			b.WriteString(" <")
			b.WriteString(a.Address)
			b.WriteString(">")
		} else {
			b.WriteString(a.Address)
		}
	}
	return b.String()
}

// containsFold reports whether substr is within s, case-insensitively.
func containsFold(s, substr string) bool {
	if substr == "" {
		return true
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
