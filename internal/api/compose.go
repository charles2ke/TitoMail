package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/charles2ke/TitoMail/internal/delivery"
	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

const (
	maxRecipients = 100
	maxScheduleAt = 366 * 24 * time.Hour
)

func htmlEscape(s string) string { return html.EscapeString(s) }

// attachmentRef refers to a file uploaded for a message being composed or to
// a part of an existing message (a forwarded attachment or one that is part
// of a saved draft).
type attachmentRef struct {
	UploadID    string `json:"upload_id,omitempty"`
	MessageID   int64  `json:"message_id,omitempty"`
	Part        string `json:"part,omitempty"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// composeState is the content of the composer, used for drafts, sending and
// reply/forward templates.
type composeState struct {
	DraftID     int64           `json:"draft_id,omitempty"`
	From        string          `json:"from"`
	To          []email.Address `json:"to"`
	Cc          []email.Address `json:"cc"`
	Bcc         []email.Address `json:"bcc"`
	Subject     string          `json:"subject"`
	HTML        string          `json:"html"`
	Attachments []attachmentRef `json:"attachments"`
	InReplyToID int64           `json:"in_reply_to_id,omitempty"`
	ForwardOfID int64           `json:"forward_of_id,omitempty"`
	ScheduleAt  *time.Time      `json:"schedule_at,omitempty"`
}

func newComposeState() *composeState {
	return &composeState{To: []email.Address{}, Cc: []email.Address{}, Bcc: []email.Address{}, Attachments: []attachmentRef{}}
}

// cleanAddrs validates and normalizes recipients. Drafts silently drop
// invalid entries; sending refuses them.
func cleanAddrs(list []email.Address, lenient bool) ([]email.Address, error) {
	out := make([]email.Address, 0, len(list))
	seen := map[string]bool{}
	for _, a := range list {
		addr := email.NormalizeAddress(a.Address)
		if !email.ValidAddress(addr) {
			if lenient {
				continue
			}
			return nil, badRequest("invalid email address: %q", a.Address)
		}
		if seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, email.Address{Name: strings.TrimSpace(a.Name), Address: addr})
	}
	return out, nil
}

func (a *API) loadAttachment(ctx context.Context, u *store.User, ref attachmentRef) (*email.OutgoingAttachment, error) {
	switch {
	case ref.UploadID != "":
		up, err := a.st.GetUpload(ctx, u.ID, ref.UploadID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, badRequest("attachment %q is no longer available; please attach it again", ref.Filename)
			}
			return nil, err
		}
		return &email.OutgoingAttachment{Filename: up.Filename, ContentType: up.ContentType, Data: up.Data}, nil
	case ref.MessageID > 0 && ref.Part != "":
		raw, _, err := a.st.MessageRaw(ctx, u.ID, ref.MessageID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, badRequest("attachment %q is no longer available", ref.Filename)
			}
			return nil, err
		}
		for _, att := range email.Parse(raw).Attachments {
			if att.PartID != ref.Part {
				continue
			}
			_, body, err := email.ExtractPart(raw, ref.Part)
			if err != nil {
				return nil, err
			}
			return &email.OutgoingAttachment{Filename: att.Filename, ContentType: att.ContentType, Data: body}, nil
		}
		return nil, badRequest("attachment %q is no longer available", ref.Filename)
	}
	return nil, badRequest("invalid attachment reference")
}

// buildOutgoing turns composer content into a message. For drafts, invalid
// recipients are dropped instead of rejected.
func (a *API) buildOutgoing(ctx context.Context, u *store.User, cs *composeState, draft bool) (*email.Outgoing, error) {
	from := email.NormalizeAddress(cs.From)
	if from == "" {
		from = u.Email
	}
	if !a.st.CanSendAs(ctx, u, from) {
		return nil, errStatus(http.StatusForbidden, "you are not allowed to send as %s", from)
	}
	o := &email.Outgoing{From: email.Address{Name: u.DisplayName, Address: from}, Subject: strings.TrimSpace(cs.Subject)}
	var err error
	if o.To, err = cleanAddrs(cs.To, draft); err != nil {
		return nil, err
	}
	if o.Cc, err = cleanAddrs(cs.Cc, draft); err != nil {
		return nil, err
	}
	if o.Bcc, err = cleanAddrs(cs.Bcc, draft); err != nil {
		return nil, err
	}
	if n := len(o.To) + len(o.Cc) + len(o.Bcc); n > maxRecipients {
		return nil, badRequest("too many recipients (%d); the limit is %d", n, maxRecipients)
	}
	o.HTML = email.SanitizeComposedHTML(cs.HTML)
	o.Text = email.HTMLToText(o.HTML)
	if cs.InReplyToID > 0 {
		if orig, err := a.st.GetMessage(ctx, u.ID, cs.InReplyToID); err == nil && orig.MessageID != "" {
			o.InReplyTo = orig.MessageID
			refs := append(append([]string{}, orig.References...), orig.MessageID)
			if len(refs) > 20 {
				refs = append(refs[:1], refs[len(refs)-19:]...)
			}
			o.References = refs
		}
	}
	size := int64(len(o.HTML) + len(o.Text))
	limit := a.opts.MaxMessageBytes * 3 / 4 // base64 encoding grows attachments by a third
	for _, ref := range cs.Attachments {
		att, err := a.loadAttachment(ctx, u, ref)
		if err != nil {
			return nil, err
		}
		size += int64(len(att.Data))
		if size > limit {
			return nil, errStatus(http.StatusRequestEntityTooLarge, "attachments are too large (limit %d MB)", limit>>20)
		}
		o.Attachments = append(o.Attachments, *att)
	}
	return o, nil
}

// --- Uploads ---

func (a *API) upload(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	r.Body = http.MaxBytesReader(w, r.Body, a.opts.MaxUploadBytes+64<<10)
	mr, err := r.MultipartReader()
	if err != nil {
		return badRequest("expected a multipart/form-data upload")
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return badRequest("no file in upload")
		}
		if err != nil {
			return err
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, a.opts.MaxUploadBytes+1))
		if err != nil {
			return err
		}
		if int64(len(data)) > a.opts.MaxUploadBytes {
			return errStatus(http.StatusRequestEntityTooLarge, "file is too large (limit %d MB)", a.opts.MaxUploadBytes>>20)
		}
		name := safeFilename(part.FileName(), "attachment")
		ct := part.Header.Get("Content-Type")
		if mt, _, err := mime.ParseMediaType(ct); err == nil && mt != "application/octet-stream" {
			ct = mt
		} else if byExt := mime.TypeByExtension(strings.ToLower(path.Ext(name))); byExt != "" {
			ct, _, _ = mime.ParseMediaType(byExt)
		} else {
			ct, _, _ = mime.ParseMediaType(http.DetectContentType(data))
		}
		up, err := a.st.CreateUpload(r.Context(), c.user.ID, name, ct, data)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusCreated, up)
	}
}

// --- Drafts ---

func (a *API) getDraft(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	raw, m, err := a.st.MessageRaw(ctx, c.user.ID, id)
	if err != nil {
		return err
	}
	if !m.Draft {
		return badRequest("this message is not a draft")
	}
	p := email.Parse(raw)
	cs := newComposeState()
	cs.DraftID = m.ID
	cs.From = p.PrimaryFrom().Address
	cs.To, cs.Cc, cs.Bcc = nonNil(p.To), nonNil(p.Cc), nonNil(p.Bcc)
	cs.Subject = p.Subject
	if p.HTML != "" {
		cs.HTML = email.BodyHTML(p.HTML)
	} else {
		cs.HTML = email.TextToHTML(p.Text)
	}
	for _, att := range p.Attachments {
		cs.Attachments = append(cs.Attachments, attachmentRef{MessageID: m.ID, Part: att.PartID,
			Filename: att.Filename, ContentType: att.ContentType, Size: att.Size})
	}
	for _, mid := range p.InReplyTo {
		found, err := a.st.FindByMessageID(ctx, c.user.ID, mid)
		if err != nil {
			return err
		}
		for _, f := range found {
			if !f.Draft {
				cs.InReplyToID = f.ID
				break
			}
		}
	}
	return ok(w, cs)
}

func (a *API) saveDraft(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var cs composeState
	if err := decode(w, r, &cs, 4<<20); err != nil {
		return err
	}
	ctx := r.Context()
	o, err := a.buildOutgoing(ctx, c.user, &cs, true)
	if err != nil {
		return err
	}
	raw, err := o.Build(true)
	if err != nil {
		return err
	}
	if err := a.st.CheckQuota(ctx, c.user, int64(len(raw))); err != nil {
		return err
	}
	drafts, err := a.st.MailboxBySpecial(ctx, c.user.ID, store.SpecialDrafts)
	if err != nil {
		return err
	}
	nm := store.NewMessage{UserID: c.user.ID, MailboxID: drafts.ID, Raw: raw, Flags: []string{store.FlagDraft, store.FlagSeen}}
	var m *store.Message
	if cs.DraftID > 0 {
		if old, err := a.st.GetMessage(ctx, c.user.ID, cs.DraftID); err == nil && old.Draft {
			m, err = a.st.ReplaceMessage(ctx, cs.DraftID, nm)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	if m == nil {
		if m, err = a.st.InsertMessage(ctx, nm); err != nil {
			return err
		}
	}
	// Attachments now live in the draft; return references to its parts in
	// the order they were submitted so the composer can update its state.
	refs := []attachmentRef{}
	for _, att := range email.Parse(raw).Attachments {
		refs = append(refs, attachmentRef{MessageID: m.ID, Part: att.PartID, Filename: att.Filename,
			ContentType: att.ContentType, Size: att.Size})
	}
	return ok(w, map[string]any{"draft_id": m.ID, "thread_id": m.ThreadID, "attachments": refs})
}

// --- Sending ---

func (a *API) send(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var cs composeState
	if err := decode(w, r, &cs, 4<<20); err != nil {
		return err
	}
	ctx := r.Context()
	o, err := a.buildOutgoing(ctx, c.user, &cs, false)
	if err != nil {
		return err
	}
	if len(o.To)+len(o.Cc)+len(o.Bcc) == 0 {
		return delivery.ErrNoRecipients
	}
	delay := time.Duration(c.user.Settings.UndoSendSeconds) * time.Second
	scheduled := false
	if cs.ScheduleAt != nil {
		d := cs.ScheduleAt.Sub(a.now())
		if d > maxScheduleAt {
			return badRequest("messages can be scheduled at most a year ahead")
		}
		if d > 0 {
			scheduled = true
			if d > delay {
				delay = d
			}
		}
	}
	draftID := int64(0)
	if cs.DraftID > 0 {
		if m, err := a.st.GetMessage(ctx, c.user.ID, cs.DraftID); err == nil && m.Draft {
			draftID = m.ID
		}
	}
	res, err := a.d.Send(ctx, delivery.SendRequest{User: c.user, Message: o, DraftID: draftID,
		ReplyToID: cs.InReplyToID, ForwardOfID: cs.ForwardOfID, Delay: delay, Scheduled: scheduled})
	if err != nil {
		return err
	}
	var uploads []string
	for _, ref := range cs.Attachments {
		if ref.UploadID != "" {
			uploads = append(uploads, ref.UploadID)
		}
	}
	if len(uploads) > 0 {
		if err := a.st.DeleteUploads(ctx, c.user.ID, uploads); err != nil {
			a.log.Warn("deleting uploads failed", "err", err)
		}
	}
	return ok(w, map[string]any{"queue_id": res.QueueID, "send_at": res.SendAt, "scheduled": scheduled,
		"message_id": res.Sent.ID, "thread_id": res.Sent.ThreadID})
}

func (a *API) listOutbox(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListQueue(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

// cancelSend implements undo send and cancelling scheduled messages: the
// message is taken out of the queue and turned back into a draft.
func (a *API) cancelSend(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	e, err := a.st.GetQueueEntry(ctx, id)
	if err != nil {
		return err
	}
	if e.UserID != c.user.ID {
		return store.ErrNotFound
	}
	raw := e.Data
	if e.SentCopyID > 0 {
		// The Sent copy still has the Bcc header.
		if sentRaw, _, err := a.st.MessageRaw(ctx, c.user.ID, e.SentCopyID); err == nil {
			raw = sentRaw
		}
	}
	if _, err := a.st.CancelQueued(ctx, c.user.ID, id); err != nil {
		return err
	}
	drafts, err := a.st.MailboxBySpecial(ctx, c.user.ID, store.SpecialDrafts)
	if err != nil {
		return err
	}
	m, err := a.st.InsertMessage(ctx, store.NewMessage{UserID: c.user.ID, MailboxID: drafts.ID, Raw: raw,
		Flags: []string{store.FlagDraft, store.FlagSeen}})
	if err != nil {
		return err
	}
	return ok(w, map[string]any{"draft_id": m.ID})
}

// --- Reply and forward templates ---

func prefixSubject(prefix, subject string) string {
	s := strings.TrimSpace(subject)
	known := []string{"fwd:", "fw:"}
	if prefix == "Re:" {
		known = []string{"re:", "aw:", "sv:"}
	}
	for _, k := range known {
		if strings.HasPrefix(strings.ToLower(s), k) {
			return s
		}
	}
	if s == "" {
		return prefix
	}
	return prefix + " " + s
}

func (a *API) composeFrom(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	raw, m, err := a.st.MessageRaw(ctx, c.user.ID, id)
	if err != nil {
		return err
	}
	p := email.Parse(raw)
	own, err := a.st.SenderAddresses(ctx, c.user)
	if err != nil {
		return err
	}
	ownSet := map[string]bool{}
	for _, addr := range own {
		ownSet[strings.ToLower(addr)] = true
	}
	isOwn := func(addr string) bool { return ownSet[strings.ToLower(addr)] }

	cs := newComposeState()
	cs.From = c.user.Email
	sender := p.PrimaryFrom()
	fromMe := isOwn(sender.Address)
	if fromMe {
		cs.From = email.NormalizeAddress(sender.Address)
	} else {
		for _, ad := range append(append(append([]email.Address{}, p.To...), p.Cc...), p.Bcc...) {
			if isOwn(ad.Address) {
				cs.From = email.NormalizeAddress(ad.Address)
				break
			}
		}
	}

	var body string
	if p.HTML != "" {
		body = email.QuoteHTML(p.HTML)
	} else {
		body = email.TextToHTML(p.Text)
	}
	from := addressHTML([]email.Address{sender})

	mode := r.URL.Query().Get("mode")
	switch mode {
	case "reply", "reply_all":
		cs.InReplyToID = m.ID
		cs.Subject = prefixSubject("Re:", p.Subject)
		seen := map[string]bool{}
		add := func(dst *[]email.Address, list []email.Address, skipOwn bool) {
			for _, ad := range list {
				key := strings.ToLower(ad.Address)
				if ad.Address == "" || seen[key] || (skipOwn && isOwn(ad.Address)) {
					continue
				}
				seen[key] = true
				*dst = append(*dst, ad)
			}
		}
		switch {
		case fromMe:
			add(&cs.To, p.To, false)
		case len(p.ReplyTo) > 0:
			add(&cs.To, p.ReplyTo, false)
		default:
			add(&cs.To, p.From, false)
		}
		if mode == "reply_all" {
			if !fromMe {
				add(&cs.To, p.To, true)
			}
			add(&cs.Cc, p.Cc, true)
		}
		cs.HTML = fmt.Sprintf(`<div class="tm_quote"><div>On %s, %s wrote:</div>`+
			`<blockquote type="cite" style="margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex">%s</blockquote></div>`,
			htmlEscape(formatQuoteDate(p.Date)), from, body)
	case "forward":
		cs.ForwardOfID = m.ID
		cs.Subject = prefixSubject("Fwd:", p.Subject)
		var hdr strings.Builder
		hdr.WriteString(`---------- Forwarded message ---------<br>`)
		fmt.Fprintf(&hdr, "From: %s<br>", from)
		fmt.Fprintf(&hdr, "Date: %s<br>", htmlEscape(formatQuoteDate(p.Date)))
		fmt.Fprintf(&hdr, "Subject: %s<br>", htmlEscape(p.Subject))
		if len(p.To) > 0 {
			fmt.Fprintf(&hdr, "To: %s<br>", addressHTML(p.To))
		}
		if len(p.Cc) > 0 {
			fmt.Fprintf(&hdr, "Cc: %s<br>", addressHTML(p.Cc))
		}
		cs.HTML = `<div class="tm_quote"><div>` + hdr.String() + `</div><br>` + body + `</div>`
		for _, att := range p.Attachments {
			if att.Inline {
				continue
			}
			cs.Attachments = append(cs.Attachments, attachmentRef{MessageID: m.ID, Part: att.PartID,
				Filename: att.Filename, ContentType: att.ContentType, Size: att.Size})
		}
	default:
		return badRequest("mode must be reply, reply_all or forward")
	}
	return ok(w, cs)
}
