package imapd

import (
	"errors"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/charles2ke/TitoMail/internal/store"
)

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if options == nil {
		options = &imap.ListOptions{}
	}
	if len(patterns) == 0 {
		// LIST "" "" returns the hierarchy delimiter.
		return w.WriteList(&imap.ListData{
			Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect},
			Delim: store.MailboxDelimiter,
		})
	}

	mailboxes, err := s.srv.st.ListMailboxes(s.ctx, s.userID())
	if err != nil {
		return s.storeErr(err)
	}
	hasChildren := make(map[string]bool)
	subscribedChild := make(map[string]bool)
	for _, mb := range mailboxes {
		for name := mb.Name; ; {
			i := strings.LastIndexByte(name, store.MailboxDelimiter)
			if i < 0 {
				break
			}
			name = name[:i]
			hasChildren[name] = true
			if mb.Subscribed {
				subscribedChild[name] = true
			}
		}
	}

	returnSubscribed := options.ReturnSubscribed || options.SelectSubscribed
	for _, mb := range mailboxes {
		if !listMatch(mb.Name, ref, patterns) {
			continue
		}
		data := &imap.ListData{Mailbox: mb.Name, Delim: store.MailboxDelimiter}
		if options.SelectSubscribed {
			childMatch := options.SelectRecursiveMatch && subscribedChild[mb.Name]
			if !mb.Subscribed && !childMatch {
				continue
			}
			if childMatch {
				data.ChildInfo = &imap.ListDataChildInfo{Subscribed: true}
			}
		}
		if options.SelectSpecialUse && specialUseAttr(mb.SpecialUse) == "" {
			continue
		}
		data.Attrs, _ = s.mailboxAttrs(mb, hasChildren)
		if !returnSubscribed {
			data.Attrs = removeAttr(data.Attrs, imap.MailboxAttrSubscribed)
		}
		if options.ReturnStatus != nil {
			data.Status, err = s.status(mb, options.ReturnStatus)
			if err != nil {
				var imapErr *imap.Error
				if errors.As(err, &imapErr) && imapErr.Code == imap.ResponseCodeNonExistent {
					continue // deleted concurrently
				}
				return err
			}
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

// mailboxAttrs returns the LIST attributes of a mailbox. If hasChildren is
// nil, the store is queried.
func (s *session) mailboxAttrs(mb *store.Mailbox, hasChildren map[string]bool) ([]imap.MailboxAttr, error) {
	var children bool
	if hasChildren != nil {
		children = hasChildren[mb.Name]
	} else {
		var err error
		children, err = s.srv.st.HasChildren(s.ctx, s.userID(), mb.Name)
		if err != nil {
			return nil, err
		}
	}
	var attrs []imap.MailboxAttr
	if children {
		attrs = append(attrs, imap.MailboxAttrHasChildren)
	} else {
		attrs = append(attrs, imap.MailboxAttrHasNoChildren)
	}
	if mb.Subscribed {
		attrs = append(attrs, imap.MailboxAttrSubscribed)
	}
	if attr := specialUseAttr(mb.SpecialUse); attr != "" {
		attrs = append(attrs, attr)
	}
	return attrs, nil
}

func specialUseAttr(special string) imap.MailboxAttr {
	switch special {
	case store.SpecialDrafts:
		return imap.MailboxAttrDrafts
	case store.SpecialSent:
		return imap.MailboxAttrSent
	case store.SpecialArchive:
		return imap.MailboxAttrArchive
	case store.SpecialJunk:
		return imap.MailboxAttrJunk
	case store.SpecialTrash:
		return imap.MailboxAttrTrash
	}
	return ""
}

func removeAttr(attrs []imap.MailboxAttr, attr imap.MailboxAttr) []imap.MailboxAttr {
	out := attrs[:0]
	for _, a := range attrs {
		if a != attr {
			out = append(out, a)
		}
	}
	return out
}

// listMatch reports whether name matches any of the LIST patterns. INBOX
// (and its children's INBOX prefix) matches case-insensitively.
func listMatch(name, ref string, patterns []string) bool {
	isInbox := name == "INBOX" || strings.HasPrefix(name, "INBOX/")
	for _, p := range patterns {
		if imapserver.MatchList(name, store.MailboxDelimiter, ref, p) {
			return true
		}
		if isInbox && imapserver.MatchList(name, store.MailboxDelimiter, upperInbox(ref), upperInbox(p)) {
			return true
		}
	}
	return false
}

// upperInbox upper-cases a leading "inbox".
func upperInbox(s string) string {
	if len(s) >= 5 && strings.EqualFold(s[:5], "INBOX") {
		return "INBOX" + s[5:]
	}
	return s
}

// normName canonicalizes the INBOX prefix of a mailbox name.
func normName(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	if len(name) > 6 && strings.EqualFold(name[:6], "INBOX/") {
		return "INBOX/" + name[6:]
	}
	return name
}

// mailboxByName looks up a mailbox; a name with an "inbox/" prefix in any
// case also finds the corresponding child of INBOX.
func (s *session) mailboxByName(name string) (*store.Mailbox, error) {
	mb, err := s.srv.st.MailboxByName(s.ctx, s.userID(), name)
	if errors.Is(err, store.ErrNotFound) {
		if n := normName(name); n != name {
			return s.srv.st.MailboxByName(s.ctx, s.userID(), n)
		}
	}
	return mb, err
}
