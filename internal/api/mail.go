package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/authres"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

// nonNil turns a nil slice into an empty one so it encodes as [].
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// --- Mailboxes ---

func (a *API) listMailboxes(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListMailboxes(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (a *API) createMailbox(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	mb, err := a.st.CreateMailbox(r.Context(), c.user.ID, req.Name)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, mb)
}

func (a *API) renameMailbox(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req struct {
		Name       *string `json:"name"`
		Subscribed *bool   `json:"subscribed"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	if req.Name != nil {
		if err := a.st.RenameMailbox(r.Context(), c.user.ID, id, *req.Name); err != nil {
			return err
		}
	}
	if req.Subscribed != nil {
		if err := a.st.SetSubscribed(r.Context(), c.user.ID, id, *req.Subscribed); err != nil {
			return err
		}
	}
	mb, err := a.st.GetMailbox(r.Context(), c.user.ID, id)
	if err != nil {
		return err
	}
	return ok(w, mb)
}

func (a *API) deleteMailbox(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	mb, err := a.st.GetMailbox(ctx, c.user.ID, id)
	if err != nil {
		return err
	}
	if mb.SpecialUse != "" {
		return badRequest("system folders cannot be deleted")
	}
	all, err := a.st.ListMailboxes(ctx, c.user.ID)
	if err != nil {
		return err
	}
	var tree []*store.Mailbox
	for _, sub := range all {
		if sub.ID == mb.ID || hasPrefixFold(sub.Name, mb.Name+"/") {
			if sub.SpecialUse != "" {
				return badRequest("the folder contains a system folder")
			}
			tree = append(tree, sub)
		}
	}
	// Unlike IMAP DELETE, deleting a folder in the web client keeps its
	// messages recoverable in Trash.
	trash, err := a.st.MailboxBySpecial(ctx, c.user.ID, store.SpecialTrash)
	if err != nil {
		return err
	}
	for _, sub := range tree {
		msgs, err := a.st.AllMessagesInMailbox(ctx, sub.ID)
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(msgs))
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
		if len(ids) > 0 {
			if _, err := a.st.MoveByID(ctx, c.user.ID, ids, trash.ID); err != nil {
				return err
			}
		}
	}
	if err := a.st.DeleteMailbox(ctx, c.user.ID, id, true); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func (a *API) emptyMailbox(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	mb, err := a.st.GetMailbox(r.Context(), c.user.ID, id)
	if err != nil {
		return err
	}
	if mb.SpecialUse != store.SpecialTrash && mb.SpecialUse != store.SpecialJunk {
		return badRequest("only Trash and Junk can be emptied")
	}
	n, err := a.st.EmptyMailbox(r.Context(), c.user.ID, id)
	if err != nil {
		return err
	}
	return ok(w, map[string]int{"deleted": n})
}

// --- Labels ---

var labelColor = regexp.MustCompile(`^(#[0-9a-fA-F]{6})?$`)

func (a *API) listLabels(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListLabels(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

type labelReq struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

func (a *API) createLabel(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req labelReq
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	if !labelColor.MatchString(req.Color) {
		return badRequest("invalid color")
	}
	l, err := a.st.CreateLabel(r.Context(), c.user.ID, req.Name, req.Color)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, l)
}

func (a *API) updateLabel(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req labelReq
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	if !labelColor.MatchString(req.Color) {
		return badRequest("invalid color")
	}
	if err := a.st.UpdateLabel(r.Context(), c.user.ID, id, req.Name, req.Color); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

func (a *API) deleteLabel(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := a.st.DeleteLabel(r.Context(), c.user.ID, id); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

// --- Listing ---

func (a *API) listConversations(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	q := r.URL.Query()
	view := store.View{Kind: q.Get("view")}
	switch view.Kind {
	case store.ViewMailbox:
		view.MailboxID = int64(queryInt(r, "mailbox", 0))
	case store.ViewLabel:
		view.LabelID = int64(queryInt(r, "label", 0))
	case store.ViewStarred, store.ViewAll:
	case "":
		view.Kind = store.ViewAll
	default:
		return badRequest("unknown view %q", view.Kind)
	}
	threaded := c.user.Settings.ConversationView
	if v := q.Get("threaded"); v != "" {
		threaded = v == "1" || v == "true"
	}
	limit := queryInt(r, "limit", c.user.Settings.PageSize)
	res, err := a.st.ListConversations(r.Context(), c.user.ID, store.ListOptions{
		View: view, Query: q.Get("q"), Offset: queryInt(r, "offset", 0), Limit: limit, Threaded: threaded,
	})
	var se *store.SearchError
	if errors.As(err, &se) {
		return badRequest("%s", se.Msg)
	}
	if err != nil {
		return err
	}
	return ok(w, res)
}

func (a *API) getThread(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	msgs, err := a.st.ThreadMessages(r.Context(), c.user.ID, id, r.URL.Query().Get("trash") == "1")
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return store.ErrNotFound
	}
	subject := ""
	for _, m := range msgs {
		if m.Subject != "" {
			subject = m.Subject
			break
		}
	}
	return ok(w, map[string]any{"thread_id": id, "subject": subject, "messages": msgs})
}

// --- Single messages ---

type authSummary struct {
	SPF      string `json:"spf,omitempty"`
	DKIM     string `json:"dkim,omitempty"`
	DMARC    string `json:"dmarc,omitempty"`
	MailedBy string `json:"mailed_by,omitempty"`
	SignedBy string `json:"signed_by,omitempty"`
}

type messageView struct {
	*store.Message
	Mailbox        string         `json:"mailbox_special"`
	Body           email.Rendered `json:"body"`
	RemoteAllowed  bool           `json:"remote_allowed"`
	SenderPref     string         `json:"sender_pref"`
	Unsubscribe    string         `json:"unsubscribe,omitempty"`
	Auth           *authSummary   `json:"auth,omitempty"`
	OriginalSender *email.Address `json:"sender,omitempty"`
}

var rasterImages = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/jpg": true, "image/gif": true, "image/webp": true,
	"image/bmp": true, "image/avif": true,
}

// inlineImages turns cid: references into data: URLs so the sandboxed
// message frame never needs to make authenticated requests.
func inlineImages(raw []byte, p *email.Message) func(string) string {
	budget := 10 << 20
	return func(cid string) string {
		att := p.AttachmentByContentID(cid)
		if att == nil {
			return ""
		}
		ct := strings.ToLower(att.ContentType)
		if !rasterImages[ct] || att.Size > int64(budget) {
			return ""
		}
		_, body, err := email.ExtractPart(raw, att.PartID)
		if err != nil || len(body) > budget {
			return ""
		}
		budget -= len(body)
		return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(body)
	}
}

func (a *API) getMessage(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	raw, m, err := a.st.MessageRaw(ctx, c.user.ID, id)
	if err != nil {
		return err
	}
	mb, err := a.st.GetMailbox(ctx, c.user.ID, m.MailboxID)
	if err != nil {
		return err
	}
	p := email.Parse(raw)
	v := &messageView{Message: m, Mailbox: mb.SpecialUse}
	v.SenderPref = a.st.GetSenderPref(ctx, c.user.ID, m.From.Address)
	switch {
	case r.URL.Query().Get("remote") == "1":
		v.RemoteAllowed = true
	case mb.SpecialUse == store.SpecialJunk:
	case mb.SpecialUse == store.SpecialSent || mb.SpecialUse == store.SpecialDrafts:
		v.RemoteAllowed = true
	case c.user.Settings.ExternalImages == "always" || v.SenderPref == store.SenderTrusted:
		v.RemoteAllowed = true
	}
	if p.HTML != "" {
		v.Body = email.RenderHTML(p.HTML, email.SanitizeOptions{AllowRemote: v.RemoteAllowed, CIDURL: inlineImages(raw, p)})
	} else {
		v.Body = email.RenderText(p.Text)
	}
	v.Unsubscribe = unsubscribeURL(p.Header.Get("List-Unsubscribe"))
	v.Auth = a.authSummary(p)
	if len(p.Sender) > 0 && len(p.From) > 0 && !strings.EqualFold(p.Sender[0].Address, p.From[0].Address) {
		v.OriginalSender = &p.Sender[0]
	}
	return ok(w, v)
}

// unsubscribeURL picks the best List-Unsubscribe target (RFC 2369).
func unsubscribeURL(h string) string {
	var mailto string
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "<") || !strings.HasSuffix(part, ">") {
			continue
		}
		u := strings.TrimSpace(part[1 : len(part)-1])
		switch lower := strings.ToLower(u); {
		case strings.HasPrefix(lower, "https://"):
			return u
		case strings.HasPrefix(lower, "mailto:") && mailto == "":
			mailto = u
		}
	}
	return mailto
}

// authSummary reports the results of the checks this server recorded when
// the message arrived. Results added by other hosts are ignored because
// anyone can forge them.
func (a *API) authSummary(p *email.Message) *authSummary {
	for _, h := range p.Header.Values("Authentication-Results") {
		id, results, err := authres.Parse(h)
		if err != nil || !strings.EqualFold(id, a.opts.Hostname) {
			continue
		}
		s := &authSummary{}
		for _, res := range results {
			switch res := res.(type) {
			case *authres.SPFResult:
				s.SPF = string(res.Value)
				if res.Value == authres.ResultPass {
					s.MailedBy = email.DomainOf(res.From)
					if s.MailedBy == "" {
						s.MailedBy = res.Helo
					}
				}
			case *authres.DKIMResult:
				if s.DKIM == "" || res.Value == authres.ResultPass {
					s.DKIM = string(res.Value)
				}
				if res.Value == authres.ResultPass && s.SignedBy == "" {
					s.SignedBy = res.Domain
				}
			case *authres.DMARCResult:
				s.DMARC = string(res.Value)
			}
		}
		return s
	}
	return nil
}

func (a *API) getRaw(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	raw, m, err := a.st.MessageRaw(r.Context(), c.user.ID, id)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if r.URL.Query().Get("download") == "1" {
		h.Set("Content-Type", "message/rfc822")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeFilename(m.Subject, "message") + ".eml"}))
	} else {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	_, err = w.Write(raw)
	return err
}

// inlineTypes may be displayed by the browser rather than downloaded.
var inlineTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true,
	"image/avif": true, "application/pdf": true, "text/plain": true,
	"audio/mpeg": true, "audio/ogg": true, "audio/wav": true, "video/mp4": true, "video/webm": true,
}

func (a *API) getAttachment(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	raw, _, err := a.st.MessageRaw(r.Context(), c.user.ID, id)
	if err != nil {
		return err
	}
	part := r.PathValue("part")
	p := email.Parse(raw)
	var att *email.Attachment
	for i := range p.Attachments {
		if p.Attachments[i].PartID == part {
			att = &p.Attachments[i]
		}
	}
	if att == nil {
		return store.ErrNotFound
	}
	_, body, err := email.ExtractPart(raw, part)
	if err != nil {
		return store.ErrNotFound
	}
	ct := strings.ToLower(att.ContentType)
	disposition := "attachment"
	if inlineTypes[ct] && r.URL.Query().Get("download") != "1" {
		disposition = "inline"
	} else {
		ct = "application/octet-stream"
	}
	if ct == "text/plain" {
		ct = "text/plain; charset=utf-8"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": safeFilename(att.Filename, "attachment")}))
	if ct != "application/pdf" {
		// Browsers refuse to run their PDF viewer in sandboxed documents.
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
	}
	h.Set("Cache-Control", "private, max-age=3600")
	_, err = w.Write(body)
	return err
}

var unsafeFilenameChars = regexp.MustCompile(`[\x00-\x1f\x7f/\\:*?"<>|]+`)

func safeFilename(name, fallback string) string {
	name = strings.TrimSpace(unsafeFilenameChars.ReplaceAllString(path.Base(strings.ReplaceAll(name, "\\", "/")), "_"))
	name = strings.Trim(name, ". ")
	if name == "" {
		return fallback
	}
	if r := []rune(name); len(r) > 150 {
		name = string(r[:150])
	}
	return name
}

// --- Actions ---

type actionReq struct {
	IDs       []int64 `json:"ids"`
	ThreadIDs []int64 `json:"thread_ids"`
	// View is the list the thread actions were taken from.
	View struct {
		Kind      string `json:"kind"`
		MailboxID int64  `json:"mailbox_id"`
		LabelID   int64  `json:"label_id"`
	} `json:"view"`
	Action    string `json:"action"`
	MailboxID int64  `json:"mailbox_id"`
	LabelID   int64  `json:"label_id"`
}

func (a *API) messageActions(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req actionReq
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	if len(req.IDs) == 0 {
		return badRequest("no messages selected")
	}
	if len(req.IDs) > 5000 {
		return badRequest("too many messages selected")
	}
	res, err := a.applyAction(r.Context(), c.user, req.IDs, &req)
	if err != nil {
		return err
	}
	return ok(w, res)
}

// threadActions applies an action to whole conversations. Flag and label
// changes cover every message of a conversation (as the list row shows the
// combined state), while moves only cover the messages in the current view,
// so that archiving a conversation from the inbox leaves Sent alone.
func (a *API) threadActions(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req actionReq
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	if len(req.ThreadIDs) == 0 {
		return badRequest("no conversations selected")
	}
	if len(req.ThreadIDs) > 1000 {
		return badRequest("too many conversations selected")
	}
	ctx := r.Context()
	view := store.View{Kind: req.View.Kind, MailboxID: req.View.MailboxID, LabelID: req.View.LabelID}
	if view.Kind == "" {
		view.Kind = store.ViewAll
	}
	inTrash := false
	if view.Kind == store.ViewMailbox {
		mb, err := a.st.GetMailbox(ctx, c.user.ID, view.MailboxID)
		if err != nil {
			return err
		}
		inTrash = mb.SpecialUse == store.SpecialTrash || mb.SpecialUse == store.SpecialJunk
	}
	var ids []int64
	for _, tid := range req.ThreadIDs {
		switch req.Action {
		case "read", "unread", "unstar", "label", "unlabel", "trash":
			msgs, err := a.st.ThreadMessages(ctx, c.user.ID, tid, inTrash)
			if err != nil {
				return err
			}
			for _, m := range msgs {
				ids = append(ids, m.ID)
			}
		case "star":
			scoped, err := a.st.ThreadScopeIDs(ctx, c.user.ID, tid, view)
			if err != nil {
				return err
			}
			if len(scoped) > 0 {
				ids = append(ids, scoped[len(scoped)-1])
			}
		default:
			scoped, err := a.st.ThreadScopeIDs(ctx, c.user.ID, tid, view)
			if err != nil {
				return err
			}
			ids = append(ids, scoped...)
		}
	}
	switch req.Action {
	case "archive", "inbox", "notspam":
		// Views that span folders (Starred, labels, All Mail, search) would
		// otherwise file the user's own Sent copies and drafts as well.
		if view.Kind != store.ViewMailbox {
			var err error
			if ids, err = a.withoutSentAndDrafts(ctx, c.user.ID, ids); err != nil {
				return err
			}
		}
	}
	if len(ids) == 0 {
		return ok(w, &actionResult{})
	}
	res, err := a.applyAction(ctx, c.user, ids, &req)
	if err != nil {
		return err
	}
	return ok(w, res)
}

// withoutSentAndDrafts drops the messages that are in Sent or Drafts.
func (a *API) withoutSentAndDrafts(ctx context.Context, userID int64, ids []int64) ([]int64, error) {
	skip := map[int64]bool{}
	for _, special := range []string{store.SpecialSent, store.SpecialDrafts} {
		mb, err := a.st.MailboxBySpecial(ctx, userID, special)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		skip[mb.ID] = true
	}
	msgs, err := a.st.GetMessages(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, m := range msgs {
		if !skip[m.MailboxID] {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// actionResult reports the outcome of an action. For moves, Undo lists the
// original folder of the moved messages so that clients can put them back.
type actionResult struct {
	Affected int        `json:"affected"`
	Undo     []undoMove `json:"undo,omitempty"`
}

type undoMove struct {
	MailboxID int64   `json:"mailbox_id"`
	IDs       []int64 `json:"ids"`
}

func (a *API) applyAction(ctx context.Context, u *store.User, ids []int64, req *actionReq) (*actionResult, error) {
	res := &actionResult{}
	flag := func(op store.FlagOp, f string) (*actionResult, error) {
		res.Affected = len(ids)
		return res, a.st.UpdateFlagsByID(ctx, u.ID, ids, op, []string{f})
	}
	moveTo := func(dst int64) (*actionResult, error) {
		msgs, err := a.st.GetMessages(ctx, u.ID, ids)
		if err != nil {
			return nil, err
		}
		n, err := a.st.MoveByID(ctx, u.ID, ids, dst)
		if err != nil {
			return nil, err
		}
		res.Affected = n
		groups := map[int64]int{}
		for _, m := range msgs {
			if m.MailboxID == dst {
				continue
			}
			i, ok := groups[m.MailboxID]
			if !ok {
				i = len(res.Undo)
				groups[m.MailboxID] = i
				res.Undo = append(res.Undo, undoMove{MailboxID: m.MailboxID})
			}
			res.Undo[i].IDs = append(res.Undo[i].IDs, m.ID)
		}
		return res, nil
	}
	move := func(special string) (*actionResult, error) {
		mb, err := a.st.MailboxBySpecial(ctx, u.ID, special)
		if err != nil {
			return nil, err
		}
		return moveTo(mb.ID)
	}
	switch req.Action {
	case "read":
		return flag(store.FlagsAdd, store.FlagSeen)
	case "unread":
		return flag(store.FlagsRemove, store.FlagSeen)
	case "star":
		return flag(store.FlagsAdd, store.FlagFlagged)
	case "unstar":
		return flag(store.FlagsRemove, store.FlagFlagged)
	case "archive":
		return move(store.SpecialArchive)
	case "trash":
		return move(store.SpecialTrash)
	case "spam":
		return move(store.SpecialJunk)
	case "notspam", "inbox":
		return move(store.SpecialInbox)
	case "move":
		if req.MailboxID <= 0 {
			return nil, badRequest("choose a folder")
		}
		return moveTo(req.MailboxID)
	case "delete":
		n, err := a.st.DeleteByID(ctx, u.ID, ids)
		res.Affected = n
		return res, err
	case "label", "unlabel":
		if req.LabelID <= 0 {
			return nil, badRequest("choose a label")
		}
		res.Affected = len(ids)
		return res, a.st.SetLabel(ctx, u.ID, ids, req.LabelID, req.Action == "label")
	}
	return nil, badRequest("unknown action %q", req.Action)
}

// formatQuoteDate formats a date like Gmail's quote header.
func formatQuoteDate(t time.Time) string {
	if t.IsZero() {
		return "an unknown date"
	}
	return t.Format("Mon, Jan 2, 2006 at 3:04 PM")
}

func addressHTML(list []email.Address) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		if a.Name != "" {
			parts = append(parts, fmt.Sprintf("%s &lt;%s&gt;", htmlEscape(a.Name), htmlEscape(a.Address)))
		} else {
			parts = append(parts, htmlEscape(a.Address))
		}
	}
	return strings.Join(parts, ", ")
}
