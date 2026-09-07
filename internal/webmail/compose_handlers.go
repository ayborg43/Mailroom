package webmail

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	netmail "net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/sociolytik/mailserver/internal/imapbackend"
)

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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

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

	rcpts := make([]string, 0, len(toAddrs)+len(ccAddrs))
	for _, a := range toAddrs {
		rcpts = append(rcpts, a.Address)
	}
	for _, a := range ccAddrs {
		rcpts = append(rcpts, a.Address)
	}

	raw := buildOutgoingMessage(s.Hostname, account.Email(), to, cc, subject, body, inReplyTo, references)

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

func buildOutgoingMessage(hostname, from, to, cc, subject, body, inReplyTo, references string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	if cc != "" {
		fmt.Fprintf(&b, "Cc: %s\r\n", cc)
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-Id: <%s@%s>\r\n", generateLocalMessageID(), hostname)
	if inReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: <%s>\r\n", strings.Trim(inReplyTo, "<>"))
	}
	if references != "" {
		fmt.Fprintf(&b, "References: %s\r\n", wrapMessageIDs(references))
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(toCRLF(body))
	return []byte(b.String())
}

func wrapMessageIDs(references string) string {
	ids := strings.Fields(references)
	wrapped := make([]string, len(ids))
	for i, id := range ids {
		wrapped[i] = "<" + strings.Trim(id, "<>") + ">"
	}
	return strings.Join(wrapped, " ")
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
