package send

import (
	"bytes"
	b64std "encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/url"
	"strings"
	"testing"

	"notifio/internal/channel"
	mailch "notifio/internal/channel/mail"
)

// A parsed request has to survive all the way into the message the relay receives.
//
// The unit tests either side of this one both passed while a non-Latin filename was being
// replaced with underscores in the parser, before the MIME encoder that exists to carry it
// ever saw the name.
func TestAFilenameSurvivesParsingAndEncoding(t *testing.T) {
	const filename = "отчёт 2026.csv"
	const content = "id,name\n1,Дом\n"

	r := multipartReq(t,
		url.Values{"to": {"ops@example.com"}, "from": {"n@example.com"},
			"subject": {"Отчёт готов"}, "body": {"see attached"}},
		[]file{{"attachments", filename, "text/csv", content}})

	req, err := Parse(r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer req.Cleanup()

	v, err := Validate(req, emailCh())
	if err != nil {
		t.Fatal(err)
	}
	if v.Atts[0].Filename != filename {
		t.Fatalf("the parser turned %q into %q", filename, v.Atts[0].Filename)
	}

	atts := make([]channel.Attachment, len(v.Atts))
	for i, a := range v.Atts {
		atts[i] = channel.Attachment{Filename: a.Filename, ContentType: a.ContentType, Size: a.Size, Open: a.Open}
	}
	m := &mailch.Message{
		From: v.From, To: v.To, Subject: v.Subject, Body: v.Body, BodyType: v.Type,
		Atts: atts, ID: "x@example.com",
	}
	var buf bytes.Buffer
	if err := m.Render(&buf); err != nil {
		t.Fatal(err)
	}

	msg, err := mail.ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Отчёт готов" {
		t.Errorf("subject arrived as %q", subject)
	}

	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	if _, err := mr.NextPart(); err != nil {
		t.Fatal(err)
	}
	part, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got := part.FileName(); got != filename {
		t.Errorf("the attachment arrived as %q, want %q", got, filename)
	}
	raw, _ := io.ReadAll(part)
	back, err := decodeB64(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if back != content {
		t.Errorf("content arrived as %q", back)
	}
}

func decodeB64(s string) (string, error) {
	clean := strings.NewReplacer("\r", "", "\n", "").Replace(s)
	raw, err := b64.DecodeString(clean)
	return string(raw), err
}

var b64 = b64std.StdEncoding
