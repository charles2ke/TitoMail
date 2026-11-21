package imapd

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"mime"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"

	"github.com/charles2ke/TitoMail/internal/store"
)

// fetchBatch is the number of messages loaded from the store at once.
const fetchBatch = 250

var (
	errNoCondStore = &imap.Error{Type: imap.StatusResponseTypeBad, Text: "CONDSTORE is not supported"}
	errTryCreate   = &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
)

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if options.ModSeq || options.ChangedSince != 0 {
		return errNoCondStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.selectedView()
	if err != nil {
		return err
	}
	if v.gone {
		return nil
	}
	idx := v.resolve(numSet)
	if len(idx) == 0 {
		return nil
	}
	uids := v.uidsAt(idx)

	markSeen := false
	if !v.readOnly {
		for _, sec := range options.BodySection {
			markSeen = markSeen || !sec.Peek
		}
		for _, sec := range options.BinarySection {
			markSeen = markSeen || !sec.Peek
		}
	}
	if markSeen {
		// The resulting flag changes are reported in the FETCH responses
		// below (the messages are loaded afterwards), so they are not
		// queued as unsolicited updates for this session.
		_, err := s.srv.st.UpdateFlags(s.ctx, v.userID, v.id, uids, store.FlagsAdd, []string{store.FlagSeen}, v.srcReported)
		if err != nil {
			return s.storeErr(err)
		}
	}

	needRaw := options.Envelope || options.BodyStructure != nil ||
		len(options.BodySection) > 0 || len(options.BinarySection) > 0 || len(options.BinarySectionSize) > 0
	for start := 0; start < len(uids); start += fetchBatch {
		end := min(start+fetchBatch, len(uids))
		msgs, err := s.srv.st.MessagesByUID(s.ctx, v.id, uids[start:end])
		if err != nil {
			return s.storeErr(err)
		}
		k := start
		for _, m := range msgs {
			if m.UserID != v.userID {
				continue
			}
			for k < end && uids[k] < m.UID {
				k++
			}
			if k == end || uids[k] != m.UID {
				continue
			}
			var raw []byte
			if needRaw {
				raw, err = s.srv.st.ReadBlob(s.ctx, m.BlobID)
				if err != nil {
					return s.internalErr(err)
				}
			}
			if err := writeFetch(w, uint32(idx[k]+1), m, raw, options, markSeen); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeFetch writes the FETCH response of one message.
func writeFetch(w *imapserver.FetchWriter, seq uint32, m *store.Message, raw []byte, options *imap.FetchOptions, withFlags bool) (err error) {
	// Compute everything before CreateMessage, which locks the connection.
	var (
		env      *imap.Envelope
		bs       imap.BodyStructure
		sections = make([][]byte, len(options.BodySection))
		binaries = make([][]byte, len(options.BinarySection))
		binSizes = make([]uint32, len(options.BinarySectionSize))
	)
	if options.Envelope {
		h, _ := splitMessage(raw)
		env = imapserver.ExtractEnvelope(h)
	}
	if options.BodyStructure != nil {
		bs = bodyStructure(raw)
	}
	for i, sec := range options.BodySection {
		sections[i] = extractBodySection(raw, sec)
	}
	for i, sec := range options.BinarySection {
		binaries[i] = imapserver.ExtractBinarySection(bytes.NewReader(raw), sec)
	}
	for i, sec := range options.BinarySectionSize {
		binSizes[i] = imapserver.ExtractBinarySectionSize(bytes.NewReader(raw), sec)
	}

	fw := w.CreateMessage(seq)
	defer func() {
		if cerr := fw.Close(); err == nil {
			err = cerr
		}
	}()
	if options.UID {
		fw.WriteUID(imap.UID(m.UID))
	}
	if options.Flags || withFlags {
		fw.WriteFlags(toIMAPFlags(m.Flags()))
	}
	if options.RFC822Size {
		fw.WriteRFC822Size(m.Size)
	}
	if options.InternalDate {
		fw.WriteInternalDate(m.ReceivedAt)
	}
	if options.Envelope {
		fw.WriteEnvelope(env)
	}
	if options.BodyStructure != nil {
		fw.WriteBodyStructure(bs)
	}
	for i, sec := range options.BodySection {
		if err := writeLiteral(fw.WriteBodySection(sec, int64(len(sections[i]))), sections[i]); err != nil {
			return err
		}
	}
	for i, sec := range options.BinarySection {
		if err := writeLiteral(fw.WriteBinarySection(sec, int64(len(binaries[i]))), binaries[i]); err != nil {
			return err
		}
	}
	for i, sec := range options.BinarySectionSize {
		fw.WriteBinarySectionSize(sec, binSizes[i])
	}
	return nil
}

func writeLiteral(wc io.WriteCloser, b []byte) error {
	_, err := wc.Write(b)
	if cerr := wc.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	if options != nil && options.UnchangedSince != 0 {
		return errNoCondStore
	}
	var op store.FlagOp
	switch flags.Op {
	case imap.StoreFlagsSet:
		op = store.FlagsSet
	case imap.StoreFlagsAdd:
		op = store.FlagsAdd
	case imap.StoreFlagsDel:
		op = store.FlagsRemove
	default:
		return &imap.Error{Type: imap.StatusResponseTypeBad, Text: "Invalid STORE operation"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.selectedView()
	if err != nil {
		return err
	}
	if v.readOnly {
		return errReadOnly
	}
	if v.gone {
		return errGone
	}
	idx := v.resolve(numSet)
	if len(idx) == 0 {
		return nil
	}
	uids := v.uidsAt(idx)
	source := v.srcReported
	if flags.Silent {
		source = v.srcSilent
	}
	newFlags, err := s.srv.st.UpdateFlags(s.ctx, v.userID, v.id, uids, op, fromIMAPFlags(flags.Flags), source)
	if err != nil {
		return s.storeErr(err)
	}
	if flags.Silent {
		return nil
	}
	_, withUID := numSet.(imap.UIDSet)
	for k, uid := range uids {
		fl, ok := newFlags[uid]
		if !ok {
			continue
		}
		fw := w.CreateMessage(uint32(idx[k] + 1))
		if withUID {
			fw.WriteUID(imap.UID(uid))
		}
		fw.WriteFlags(toIMAPFlags(fl))
		if err := fw.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.selectedView()
	if err != nil {
		return nil, err
	}
	if v.gone {
		return nil, errGone
	}
	dst, err := s.mailboxByName(dest)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errTryCreate
	} else if err != nil {
		return nil, s.storeErr(err)
	}
	idx := v.resolve(numSet)
	if len(idx) == 0 {
		return nil, nil
	}
	srcUIDs, dstUIDs, err := s.srv.st.CopyMessages(s.ctx, v.userID, v.id, v.uidsAt(idx), dst.ID, nil)
	if err != nil {
		return nil, s.storeErr(err)
	}
	if len(srcUIDs) == 0 {
		return nil, nil
	}
	return &imap.CopyData{
		UIDValidity: dst.UIDValidity,
		SourceUIDs:  uidSet(srcUIDs),
		DestUIDs:    uidSet(dstUIDs),
	}, nil
}

func (s *session) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.selectedView()
	if err != nil {
		return err
	}
	if v.readOnly {
		return errReadOnly
	}
	if v.gone {
		return errGone
	}
	dst, err := s.mailboxByName(dest)
	if errors.Is(err, store.ErrNotFound) {
		return errTryCreate
	} else if err != nil {
		return s.storeErr(err)
	}
	if dst.ID == v.id {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "Source and destination mailboxes are the same"}
	}
	idx := v.resolve(numSet)
	if len(idx) == 0 {
		return nil
	}
	srcUIDs, dstUIDs, err := s.srv.st.MoveMessages(s.ctx, v.userID, v.id, v.uidsAt(idx), dst.ID, v.srcReported)
	if err != nil {
		return s.storeErr(err)
	}
	if len(srcUIDs) == 0 {
		return nil
	}
	// The EXPUNGE responses are sent by the Poll that follows the command,
	// from the store's change notification.
	return w.WriteCopyData(&imap.CopyData{
		UIDValidity: dst.UIDValidity,
		SourceUIDs:  uidSet(srcUIDs),
		DestUIDs:    uidSet(dstUIDs),
	})
}

func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.selectedView()
	if err != nil {
		return err
	}
	if v.readOnly {
		return errReadOnly
	}
	if v.gone {
		return nil
	}
	var list []uint32 // nil: all messages flagged \Deleted
	if uids != nil {
		idx := v.resolve(*uids)
		if len(idx) == 0 {
			return nil
		}
		list = v.uidsAt(idx)
	}
	// EXPUNGE responses are sent by the Poll that follows the command.
	if _, err := s.srv.st.Expunge(s.ctx, v.userID, v.id, list, v.srcReported); err != nil {
		return s.storeErr(err)
	}
	return nil
}

func (s *session) internalErr(err error) error {
	s.srv.log.Warn("imap: internal error", "err", err, "remote", s.remote)
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeServerBug, Text: "Internal server error"}
}

// headerEnd returns the offset of the message body: just after the first
// empty line, or len(b) if there is none.
func headerEnd(b []byte) int {
	for i := 0; i < len(b); {
		j := bytes.IndexByte(b[i:], '\n')
		if j < 0 {
			return len(b)
		}
		line := b[i : i+j+1]
		i += j + 1
		if len(line) == 1 || (len(line) == 2 && line[0] == '\r') {
			return i
		}
	}
	return len(b)
}

// splitMessage splits a message into its (possibly partially parsed, if
// malformed) header and its body.
func splitMessage(raw []byte) (textproto.Header, []byte) {
	end := headerEnd(raw)
	h, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw[:end])))
	return h, raw[end:]
}

// extractBodySection returns a BODY[...] section of a message. Top-level
// sections are taken directly from the raw message.
func extractBodySection(raw []byte, sec *imap.FetchItemBodySection) []byte {
	if len(sec.Part) == 0 {
		end := headerEnd(raw)
		switch sec.Specifier {
		case imap.PartSpecifierNone:
			return extractPartial(raw, sec.Partial)
		case imap.PartSpecifierText:
			return extractPartial(raw[end:], sec.Partial)
		case imap.PartSpecifierHeader:
			if len(sec.HeaderFields) == 0 && len(sec.HeaderFieldsNot) == 0 {
				return extractPartial(raw[:end], sec.Partial)
			}
			return extractPartial(filterHeader(raw[:end], sec.HeaderFields, sec.HeaderFieldsNot), sec.Partial)
		}
	}
	return imapserver.ExtractBodySection(bytes.NewReader(raw), sec)
}

func filterHeader(raw []byte, fields, notFields []string) []byte {
	h, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if len(fields) > 0 {
		keep := make(map[string]bool, len(fields))
		for _, k := range fields {
			keep[strings.ToLower(k)] = true
		}
		for f := h.Fields(); f.Next(); {
			if !keep[strings.ToLower(f.Key())] {
				f.Del()
			}
		}
	}
	for _, k := range notFields {
		h.Del(k)
	}
	var buf bytes.Buffer
	if err := textproto.WriteHeader(&buf, h); err != nil {
		return nil
	}
	return buf.Bytes()
}

func extractPartial(b []byte, partial *imap.SectionPartial) []byte {
	if partial == nil {
		return b
	}
	if partial.Offset > int64(len(b)) {
		return nil
	}
	size := int64(len(b)) - partial.Offset
	if partial.Size >= 0 && partial.Size < size {
		size = partial.Size
	}
	return b[partial.Offset : partial.Offset+size]
}

// maxBodyDepth limits the nesting of multiparts and attached messages
// described in BODYSTRUCTURE responses.
const maxBodyDepth = 32

// bodyStructure computes the (extended) body structure of a message. It
// always returns a structure that can be written, even for malformed
// messages.
func bodyStructure(raw []byte) imap.BodyStructure {
	h, body := splitMessage(raw)
	return buildBodyStructure(h, body, "", 0)
}

func buildBodyStructure(h textproto.Header, body []byte, parentType string, depth int) imap.BodyStructure {
	hdr := gomessage.Header{Header: h}
	mediaType, params := contentType(hdr, parentType)
	isMessage := mediaType == "message/rfc822" || mediaType == "message/global"
	if depth >= maxBodyDepth && (isMessage || strings.HasPrefix(mediaType, "multipart/")) {
		mediaType, params, isMessage = "application/octet-stream", nil, false
	}
	typ, subtype, _ := strings.Cut(mediaType, "/")

	if typ == "multipart" {
		bs := &imap.BodyStructureMultiPart{
			Subtype: subtype,
			Extended: &imap.BodyStructureMultiPartExt{
				Params:      params,
				Disposition: contentDisposition(hdr),
				Language:    contentLanguage(hdr),
				Location:    hdr.Get("Content-Location"),
			},
		}
		mr := textproto.NewMultipartReader(bytes.NewReader(body), params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			pb, err := io.ReadAll(p)
			if err != nil {
				break
			}
			bs.Children = append(bs.Children, buildBodyStructure(p.Header, pb, mediaType, depth+1))
		}
		if len(bs.Children) == 0 {
			// A multipart must have at least one part.
			bs.Children = []imap.BodyStructure{&imap.BodyStructureSinglePart{
				Type:     "text",
				Subtype:  "plain",
				Params:   map[string]string{"charset": "us-ascii"},
				Text:     &imap.BodyStructureText{},
				Extended: &imap.BodyStructureSinglePartExt{},
			}}
		}
		return bs
	}

	bs := &imap.BodyStructureSinglePart{
		Type:        typ,
		Subtype:     subtype,
		Params:      params,
		ID:          hdr.Get("Content-Id"),
		Description: hdr.Get("Content-Description"),
		Encoding:    strings.TrimSpace(hdr.Get("Content-Transfer-Encoding")),
		Size:        uint32(len(body)),
		Extended: &imap.BodyStructureSinglePartExt{
			Disposition: contentDisposition(hdr),
			Language:    contentLanguage(hdr),
			Location:    hdr.Get("Content-Location"),
		},
	}
	numLines := int64(bytes.Count(body, []byte("\n")))
	switch {
	case isMessage:
		ch, cb := splitMessage(body)
		bs.MessageRFC822 = &imap.BodyStructureMessageRFC822{
			Envelope:      imapserver.ExtractEnvelope(ch),
			BodyStructure: buildBodyStructure(ch, cb, "", depth+1),
			NumLines:      numLines,
		}
	case typ == "text":
		bs.Text = &imap.BodyStructureText{NumLines: numLines}
	}
	return bs
}

// contentType returns the lower-case media type and parameters of a part,
// applying the RFC 2045/2046 defaults for missing or invalid values.
func contentType(h gomessage.Header, parentType string) (string, map[string]string) {
	defaultType := "text/plain"
	if parentType == "multipart/digest" {
		defaultType = "message/rfc822"
	}
	if !h.Has("Content-Type") {
		if defaultType == "text/plain" {
			return defaultType, map[string]string{"charset": "us-ascii"}
		}
		return defaultType, nil
	}
	t, params, err := h.ContentType()
	if err != nil {
		var perr error
		t, _, perr = mime.ParseMediaType(h.Get("Content-Type"))
		if perr != nil && !errors.Is(perr, mime.ErrInvalidMediaParameter) {
			t = ""
		}
		params = nil
	}
	t = strings.ToLower(t)
	if typ, sub, ok := strings.Cut(t, "/"); !ok || typ == "" || sub == "" {
		return "text/plain", map[string]string{"charset": "us-ascii"}
	}
	return t, params
}

func contentDisposition(h gomessage.Header) *imap.BodyStructureDisposition {
	v := h.Get("Content-Disposition")
	if v == "" {
		return nil
	}
	disp, params, err := h.ContentDisposition()
	if err != nil {
		var perr error
		disp, _, perr = mime.ParseMediaType(v)
		if perr != nil && !errors.Is(perr, mime.ErrInvalidMediaParameter) {
			return nil
		}
		params = nil
	}
	if disp == "" {
		return nil
	}
	return &imap.BodyStructureDisposition{Value: strings.ToLower(disp), Params: params}
}

func contentLanguage(h gomessage.Header) []string {
	v := h.Get("Content-Language")
	if v == "" {
		return nil
	}
	var out []string
	for _, lang := range strings.Split(v, ",") {
		if lang = strings.TrimSpace(lang); lang != "" {
			out = append(out, lang)
		}
	}
	return out
}
