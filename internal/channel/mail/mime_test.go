package mail

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

	"notifio/internal/channel"
)

func att(name, ctype, body string) channel.Attachment {
	return channel.Attachment{
		Filename: name, ContentType: ctype, Size: int64(len(body)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil },
	}
}

func render(t *testing.T, m *Message) string {
	t.Helper()
	m.ID = "abc123@example.com"
	m.Date = time.Date(2026, 9, 12, 4, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := m.Render(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// A message without these is accepted by a permissive relay and scored as spam downstream.
func TestTheGeneratedHeadersArePresent(t *testing.T) {
	got := render(t, &Message{From: "n@example.com", To: []string{"a@x.com"}, Subject: "Hi", Body: "b"})
	for _, h := range []string{"Date: ", "Message-ID: <abc123@example.com>", "MIME-Version: 1.0"} {
		if !strings.Contains(got, h) {
			t.Errorf("missing %q in:\n%s", h, got)
		}
	}
}

func TestParsesAsAMessage(t *testing.T) {
	got := render(t, &Message{From: "Notifio <n@example.com>", To: []string{"a@x.com", "b@x.com"},
		Subject: "Hi", Body: "hello"})
	m, err := mail.ReadMessage(strings.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if to := m.Header.Get("To"); to != "a@x.com, b@x.com" {
		t.Errorf("To = %q", to)
	}
	if ct := m.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// 2047 for the subject, 2231 for the filename, through a real parser.
func TestNonASCIIRoundTrips(t *testing.T) {
	const subject = "Диск заполнен на 91% — срочно"
	const filename = "отчёт.csv"
	got := render(t, &Message{
		From: "Отправитель <n@example.com>", To: []string{"a@x.com"},
		Subject: subject, Body: "b", Atts: []channel.Attachment{att(filename, "text/csv", "1,2")},
	})

	m, err := mail.ReadMessage(strings.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	back, err := dec.DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if back != subject {
		t.Errorf("subject came back %q, want %q", back, subject)
	}
	if raw := m.Header.Get("Subject"); strings.Contains(raw, subject) {
		t.Error("the subject went out unencoded")
	}

	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	mr.NextPart() // the body
	part, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got := part.FileName(); got != filename {
		t.Errorf("filename came back %q, want %q", got, filename)
	}
}

func TestAttachmentStructure(t *testing.T) {
	got := render(t, &Message{From: "n@example.com", To: []string{"a@x.com"}, Subject: "s",
		Body: "<b>hi</b>", BodyType: "html",
		Atts: []channel.Attachment{att("a.txt", "text/plain", "one"), att("b.csv", "text/csv", "two")}})

	m, _ := mail.ReadMessage(strings.NewReader(got))
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mt != "multipart/mixed" {
		t.Fatalf("Content-Type = %q, want multipart/mixed", mt)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])

	first, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if ct := first.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("the body part is %q, want text/html", ct)
	}
	for _, want := range []string{"a.txt", "b.csv"} {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		if p.FileName() != want {
			t.Errorf("attachment = %q, want %q", p.FileName(), want)
		}
		if enc := p.Header.Get("Content-Transfer-Encoding"); enc != "base64" {
			t.Errorf("%s encoding = %q", want, enc)
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Error("there is a part after the attachments")
	}
}

// RFC 5322 caps a line at 998 octets. Gmail accepts one long line; strict relays do not.
func TestBase64IsWrapped(t *testing.T) {
	big := strings.Repeat("payload-", 4000)
	got := render(t, &Message{From: "n@example.com", To: []string{"a@x.com"}, Subject: "s", Body: "b",
		Atts: []channel.Attachment{att("big.bin", "application/octet-stream", big)}})

	for i, line := range strings.Split(got, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line %d is %d octets", i, len(line))
		}
	}
	if !strings.Contains(got, "\r\n") {
		t.Error("the message does not use CRLF")
	}
}

// The attachment has to survive the encoding, not merely be wrapped by it.
func TestAttachmentContentSurvives(t *testing.T) {
	const content = "id,name\n1,Дом\n2,Sea\n"
	got := render(t, &Message{From: "n@example.com", To: []string{"a@x.com"}, Subject: "s", Body: "b",
		Atts: []channel.Attachment{att("data.csv", "text/csv", content)}})

	m, _ := mail.ReadMessage(strings.NewReader(got))
	_, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	mr := multipart.NewReader(m.Body, params["boundary"])
	mr.NextPart()
	part, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	// NextPart does not decode base64; do it the way a mail client would.
	raw, _ := io.ReadAll(part)
	decoded, err := decodeBase64(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if decoded != content {
		t.Errorf("content came back %q, want %q", decoded, content)
	}
}

func TestBodyIsQuotedPrintable(t *testing.T) {
	got := render(t, &Message{From: "n@example.com", To: []string{"a@x.com"}, Subject: "s",
		Body: "90% full — " + strings.Repeat("long ", 300)})
	if !strings.Contains(got, "Content-Transfer-Encoding: quoted-printable") {
		t.Error("the body is not quoted-printable")
	}
	for _, line := range strings.Split(got, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a body line is %d octets", len(line))
		}
	}
}

func TestEnvelopeIsBareAddresses(t *testing.T) {
	from, rcpt, err := Envelope("Notifio <n@example.com>", []string{`"Doe, Jane" <jane@x.com>`})
	if err != nil {
		t.Fatal(err)
	}
	if from != "n@example.com" {
		t.Errorf("MAIL FROM = %q", from)
	}
	if len(rcpt) != 1 || rcpt[0] != "jane@x.com" {
		t.Errorf("RCPT TO = %v", rcpt)
	}
}

func TestNewIDTakesItsDomainFromFrom(t *testing.T) {
	id, err := NewID("Notifio <n@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(id, "@example.com") {
		t.Errorf("Message-ID = %q", id)
	}
}

func decodeBase64(s string) (string, error) {
	clean := strings.NewReplacer("\r", "", "\n", "").Replace(s)
	raw, err := base64.StdEncoding.DecodeString(clean)
	return string(raw), err
}
