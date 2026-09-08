package webmail

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	netmail "net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message/mail"

	"github.com/sociolytik/mailserver/internal/imapbackend"
)

// maxComposeRequestSize caps a compose submission's total size (body +
// every attachment), matching the sort of limit most mail providers apply.
const maxComposeRequestSize = 25 << 20 // 25 MiB

// composeAttachment is one file uploaded alongside a composed message,
// read fully into memory (bounded by maxComposeRequestSize).
type composeAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

type composeData struct {
	To, Cc, Subject, Body string
	InReplyTo, References string
	Error                 string
}

func (s *Server) handleComposeForm(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	data := &composeData{}

	mode := r.URL.Query().Get("mode")
	folder := r.URL.Query().Get("folder")
	uidStr := r.URL.Query().Get("uid")

	if mode != "" && folder != "" && uidStr != "" {
		if uid64, err := strconv.ParseUint(uidStr, 10, 32); err == nil {
			if original, err := account.GetMessage(folder, uint32(uid64)); err == nil {
				fillComposeDefaults(data, account.Email(), mode, original)
			}
		}
	}

	render(w, "compose_page", data)
}

func fillComposeDefaults(data *composeData, selfEmail, mode string, original *imapbackend.FullMessage) {
	switch mode {
	case "reply", "replyall":
		data.To = original.From
		if mode == "replyall" {
			data.Cc = replyAllCc(selfEmail, original)
		}
		data.Subject = ensurePrefix(original.Subject, "Re: ")
		data.Body = quoteBody(original)
		data.InReplyTo = original.MessageID
		data.References = appendReference(original.References, original.MessageID)
	case "forward":
		data.Subject = ensurePrefix(original.Subject, "Fwd: ")
		data.Body = forwardBody(original)
	}
}

// replyAllCc combines the original To/Cc into a Cc list, dropping the
// current user and the original sender (who's already the To).
func replyAllCc(selfEmail string, original *imapbackend.FullMessage) string {
	exclude := map[string]bool{
		strings.ToLower(extractAddr(selfEmail)):     true,
		strings.ToLower(extractAddr(original.From)): true,
	}
	var kept []string
	for _, addr := range splitAddressList(original.To, original.Cc) {
		if !exclude[strings.ToLower(extractAddr(addr))] {
			kept = append(kept, addr)
		}
	}
	return strings.Join(kept, ", ")
}

func ensurePrefix(subject, prefix string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(subject)), strings.ToLower(strings.TrimSpace(prefix))) {
		return subject
	}
	return prefix + subject
}

func appendReference(references, messageID string) string {
	if messageID == "" {
		return references
	}
	if references == "" {
		return messageID
	}
	return references + " " + messageID
}

func quoteBody(original *imapbackend.FullMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "On %s, %s wrote:\n", original.Date.Local().Format("Jan 2, 2006 at 3:04 PM"), original.From)

	text := original.TextBody
	if text == "" && original.HTMLBody != "" {
		text = "(original message was HTML-only)"
	}
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("> ")
		b.WriteString(strings.TrimRight(line, "\r"))
		b.WriteString("\n")
	}
	return b.String()
}

func forwardBody(original *imapbackend.FullMessage) string {
	var b strings.Builder
	b.WriteString("---------- Forwarded message ----------\n")
	fmt.Fprintf(&b, "From: %s\n", original.From)
	fmt.Fprintf(&b, "Date: %s\n", original.Date.Local().Format("Jan 2, 2006 at 3:04 PM"))
	fmt.Fprintf(&b, "Subject: %s\n", original.Subject)
	fmt.Fprintf(&b, "To: %s\n\n", original.To)

	switch {
	case original.TextBody != "":
		b.WriteString(original.TextBody)
	case original.HTMLBody != "":
		b.WriteString("(original message was HTML-only; open it directly to view)")
	}
	return b.String()
}

func extractAddr(s string) string {
	addr, err := netmail.ParseAddress(s)
	if err != nil {
		return s
	}
	return addr.Address
}

// splitAddressList parses one or more comma-separated address-list strings
// and returns each address formatted as "Name <addr>" (or bare "addr").
func splitAddressList(lists ...string) []string {
	joined := strings.Join(lists, ", ")
	parsed, err := netmail.ParseAddressList(joined)
	if err != nil {
		return nil
	}
	out := make([]string, len(parsed))
	for i, a := range parsed {
		if a.Name != "" {
			out[i] = fmt.Sprintf("%s <%s>", a.Name, a.Address)
		} else {
			out[i] = a.Address
		}
	}
	return out
}

func (s *Server) handleComposeSubmit(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	r.Body = http.MaxBytesReader(w, r.Body, maxComposeRequestSize)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		render(w, "compose_page", &composeData{Error: "Message too large (max 25 MB including attachments)"})
		return
	}
	defer r.MultipartForm.RemoveAll()

	// Header values come straight from user input. A bare CR or LF here
	// would let an attacker inject arbitrary extra headers (or even smuggle
	// a fake body) into the outgoing message — the email equivalent of HTTP
	// response splitting. Strip them before these values ever touch a
	// header line.
	to := sanitizeHeaderValue(strings.TrimSpace(r.FormValue("to")))
	cc := sanitizeHeaderValue(strings.TrimSpace(r.FormValue("cc")))
	subject := sanitizeHeaderValue(r.FormValue("subject"))
	inReplyTo := sanitizeHeaderValue(r.FormValue("in_reply_to"))
	references := sanitizeHeaderValue(r.FormValue("references"))
	body := r.FormValue("body")

	data := &composeData{To: to, Cc: cc, Subject: subject, Body: body, InReplyTo: inReplyTo, References: references}

	if to == "" {
		data.Error = "At least one recipient is required"
		render(w, "compose_page", data)
		return
	}

	toAddrs, err := netmail.ParseAddressList(to)
	if err != nil {
		data.Error = "Invalid To address: " + err.Error()
		render(w, "compose_page", data)
		return
	}
	var ccAddrs []*netmail.Address
	if cc != "" {
		ccAddrs, err = netmail.ParseAddressList(cc)
		if err != nil {
			data.Error = "Invalid Cc address: " + err.Error()
			render(w, "compose_page", data)
			return
		}
	}

	var attachments []composeAttachment
	for _, fh := range r.MultipartForm.File["attachments"] {
		f, err := fh.Open()
		if err != nil {
			data.Error = "Failed to read attachment " + fh.Filename
			render(w, "compose_page", data)
			return
		}
		content, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			data.Error = "Failed to read attachment " + fh.Filename
			render(w, "compose_page", data)
			return
		}
		ct := fh.Header.Get("Content-Type")
		if ct == "" {
			ct = http.DetectContentType(content)
		}
		attachments = append(attachments, composeAttachment{Filename: fh.Filename, ContentType: ct, Data: content})
	}

	rcpts := make([]string, 0, len(toAddrs)+len(ccAddrs))
	for _, a := range toAddrs {
		rcpts = append(rcpts, a.Address)
	}
	for _, a := range ccAddrs {
		rcpts = append(rcpts, a.Address)
	}

	raw, err := buildOutgoingMessage(s.Hostname, account.Email(), toAddrs, ccAddrs, subject, body, inReplyTo, references, attachments)
	if err != nil {
		s.Logger.Error("building composed message", "from", account.Email(), "error", err)
		data.Error = "Failed to build message: " + err.Error()
		render(w, "compose_page", data)
		return
	}

	sent, err := s.Sender.Send(account.Email(), rcpts, raw)
	if err != nil {
		s.Logger.Error("sending composed message", "from", account.Email(), "error", err)
		data.Error = "Failed to send: " + err.Error()
		render(w, "compose_page", data)
		return
	}

	if _, err := account.Append("Sent", sent, []string{`\Seen`}); err != nil {
		// The message was sent successfully; losing the Sent copy isn't
		// worth failing the request over, but it is worth logging.
		s.Logger.Error("saving sent copy", "from", account.Email(), "error", err)
	}

	http.Redirect(w, r, "/mail/Sent", http.StatusSeeOther)
}

func sanitizeHeaderValue(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// buildOutgoingMessage builds a mail.mime message: multipart/mixed with one
// text/plain part plus one part per attachment (multipart/mixed with a
// single text part when there are none — a normal, common message shape,
// not a special case the library needs to avoid).
func buildOutgoingMessage(hostname, from string, to, cc []*netmail.Address, subject, body, inReplyTo, references string, attachments []composeAttachment) ([]byte, error) {
	var h mail.Header
	h.SetAddressList("From", []*mail.Address{{Address: from}})
	h.SetAddressList("To", to)
	if len(cc) > 0 {
		h.SetAddressList("Cc", cc)
	}
	h.SetSubject(subject)
	h.SetDate(time.Now())
	h.SetMessageID(generateLocalMessageID() + "@" + hostname)
	if inReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{strings.Trim(inReplyTo, "<>")})
	}
	if references != "" {
		h.SetMsgIDList("References", strings.Fields(references))
	}

	var buf strings.Builder
	mw, err := mail.CreateWriter(&buf, h)
	if err != nil {
		return nil, fmt.Errorf("creating writer: %w", err)
	}

	var th mail.InlineHeader
	th.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	tw, err := mw.CreateSingleInline(th)
	if err != nil {
		return nil, fmt.Errorf("creating text part: %w", err)
	}
	if _, err := tw.Write([]byte(toCRLF(body))); err != nil {
		return nil, fmt.Errorf("writing text part: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing text part: %w", err)
	}

	for _, att := range attachments {
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		var ah mail.AttachmentHeader
		ah.SetContentType(ct, nil)
		ah.SetFilename(att.Filename)
		aw, err := mw.CreateAttachment(ah)
		if err != nil {
			return nil, fmt.Errorf("creating attachment %q: %w", att.Filename, err)
		}
		if _, err := aw.Write(att.Data); err != nil {
			return nil, fmt.Errorf("writing attachment %q: %w", att.Filename, err)
		}
		if err := aw.Close(); err != nil {
			return nil, fmt.Errorf("closing attachment %q: %w", att.Filename, err)
		}
	}

	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("closing writer: %w", err)
	}
	return []byte(buf.String()), nil
}

func generateLocalMessageID() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("%d.%s", time.Now().UnixNano(), hex.EncodeToString(buf))
}

func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
