package mail

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"notifio/internal/channel"
)

// base64LineLen keeps every line inside RFC 5322's 998-octet cap. Gmail accepts one long
// line; strict relays do not.
const base64LineLen = 76

// Message is one message, ready to write.
type Message struct {
	From     string
	To       []string
	Subject  string
	Body     string
	BodyType string
	Atts     []channel.Attachment

	// ID is the Message-ID, without angle brackets.
	ID string
	// Date is the Date header. Zero means now.
	Date time.Time
}

// NewID mints a Message-ID local part, with the domain taken from a From address.
func NewID(from string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	domain := "notifio.invalid"
	if a, err := mail.ParseAddress(from); err == nil {
		if i := strings.LastIndex(a.Address, "@"); i >= 0 {
			domain = a.Address[i+1:]
		}
	}
	return hex.EncodeToString(raw) + "@" + domain, nil
}

// Render writes the message as RFC 5322 bytes.
func (m *Message) Render(w io.Writer) error {
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}

	head := textproto.MIMEHeader{}
	head.Set("From", encodeAddress(m.From))
	head.Set("To", strings.Join(encodeAddresses(m.To), ", "))
	head.Set("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	head.Set("Date", date.UTC().Format(time.RFC1123Z))
	head.Set("Message-ID", "<"+m.ID+">")
	head.Set("MIME-Version", "1.0")

	if len(m.Atts) == 0 {
		head.Set("Content-Type", m.contentType())
		head.Set("Content-Transfer-Encoding", "quoted-printable")
		if err := writeHeader(w, head, []string{"From", "To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type", "Content-Transfer-Encoding"}); err != nil {
			return err
		}
		return writeQuotedPrintable(w, m.Body)
	}

	mw := multipart.NewWriter(w)
	head.Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	if err := writeHeader(w, head, []string{"From", "To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type"}); err != nil {
		return err
	}

	bodyPart, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {m.contentType()},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return err
	}
	if err := writeQuotedPrintable(bodyPart, m.Body); err != nil {
		return err
	}

	for _, a := range m.Atts {
		// FormatMediaType writes RFC 2231 continuations, which is what makes a non-ASCII
		// filename survive.
		disp := mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
		ctype := mime.FormatMediaType(a.ContentType, nil)
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		part, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {ctype},
			"Content-Transfer-Encoding": {"base64"},
			"Content-Disposition":       {disp},
		})
		if err != nil {
			return err
		}
		if err := writeBase64(part, a); err != nil {
			return fmt.Errorf("attachment %q: %w", a.Filename, err)
		}
	}
	return mw.Close()
}

func (m *Message) contentType() string {
	if m.BodyType == "html" {
		return "text/html; charset=utf-8"
	}
	return "text/plain; charset=utf-8"
}

func writeHeader(w io.Writer, head textproto.MIMEHeader, order []string) error {
	var b strings.Builder
	for _, k := range order {
		if v := head.Get(k); v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func writeQuotedPrintable(w io.Writer, s string) error {
	// CRLF line endings, because a bare LF in DATA is not a line ending to every relay.
	qp := quotedprintable.NewWriter(w)
	if _, err := io.WriteString(qp, strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")); err != nil {
		qp.Close()
		return err
	}
	return qp.Close()
}

func writeBase64(w io.Writer, a channel.Attachment) error {
	rc, err := a.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	lw := &lineWrapper{w: w, width: base64LineLen}
	enc := base64.NewEncoder(base64.StdEncoding, lw)
	if _, err := io.Copy(enc, rc); err != nil {
		enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return lw.Close()
}

// lineWrapper breaks a stream into CRLF-terminated lines of at most width.
type lineWrapper struct {
	w     io.Writer
	width int
	n     int
}

func (l *lineWrapper) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		room := l.width - l.n
		if room <= 0 {
			if _, err := l.w.Write([]byte("\r\n")); err != nil {
				return written, err
			}
			l.n = 0
			continue
		}
		chunk := min(room, len(p))
		n, err := l.w.Write(p[:chunk])
		written += n
		l.n += n
		p = p[n:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (l *lineWrapper) Close() error {
	if l.n == 0 {
		return nil
	}
	_, err := l.w.Write([]byte("\r\n"))
	return err
}

func encodeAddress(raw string) string {
	a, err := mail.ParseAddress(raw)
	if err != nil {
		return raw
	}
	if a.Name == "" {
		return a.Address
	}
	return mime.QEncoding.Encode("utf-8", a.Name) + " <" + a.Address + ">"
}

func encodeAddresses(raw []string) []string {
	out := make([]string, len(raw))
	for i, r := range raw {
		out[i] = encodeAddress(r)
	}
	return out
}

// Envelope returns the bare addresses for MAIL FROM and RCPT TO.
func Envelope(from string, to []string) (string, []string, error) {
	f, err := mail.ParseAddress(from)
	if err != nil {
		return "", nil, err
	}
	rcpt := make([]string, 0, len(to))
	for _, t := range to {
		a, err := mail.ParseAddress(t)
		if err != nil {
			return "", nil, err
		}
		rcpt = append(rcpt, a.Address)
	}
	return f.Address, rcpt, nil
}
