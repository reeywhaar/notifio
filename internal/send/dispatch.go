package send

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"notifio/internal/app"

	"notifio/internal/channel"
	"notifio/internal/channel/mail"
	"notifio/internal/channel/telegram"
	"notifio/internal/config"
)

// SenderFor returns the sender for a configured channel. Adding a type is one case here.
func SenderFor(cfg *config.Channel, client *http.Client) (channel.Sender, error) {
	switch cfg.Type {
	case config.TypeTelegram:
		return telegram.New(cfg, client), nil
	case config.TypeEmail:
		return mail.New(cfg), nil
	}
	return nil, fmt.Errorf("channel %q has type %q", cfg.Name, cfg.Type)
}

// Deliver sends a validated request through its channel, bounded by that channel's timeout.
//
// The same path serves the HTTP handler and `notifio channel test`, so the command exercises
// what the endpoint does rather than something beside it.
func Deliver(ctx context.Context, v *Valid, client *http.Client) (*channel.Result, error) {
	s, err := SenderFor(v.Channel, client)
	if err != nil {
		return nil, err
	}

	atts := make([]channel.Attachment, len(v.Atts))
	for i, a := range v.Atts {
		atts[i] = channel.Attachment{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			Size:        a.Size,
			Open:        a.Open,
		}
	}

	ctx, cancel := context.WithTimeout(ctx, v.Channel.SendTimeout.D())
	defer cancel()

	return s.Send(ctx, &channel.Message{
		To:          v.To,
		From:        v.From,
		Subject:     v.Subject,
		Body:        v.Body,
		BodyType:    v.Type,
		Atts:        atts,
		LinkPreview: v.LinkPreview,
	})
}

// TestMessage is what `notifio channel test` sends.
//
// It goes through Validate, so the command exercises the same refusals the endpoint does, and
// it is marked up by default: a plain test message proves the connection and says nothing about
// whether parse_mode or an HTML part survives the round trip, which is the half that breaks
// against a real provider.
func TestMessage(ch *config.Channel, to, subject, bodyType string) (*Valid, error) {
	req := &Request{present: map[string]bool{}}
	req.present["body"] = true
	req.present["body_type"] = true
	req.BodyType = bodyType
	req.Body = testBody(ch, bodyType, time.Now().UTC())
	if ch.Type == config.TypeTelegram && ch.Pinned.LinkPreview == nil {
		// The body is mostly a link, and a preview card over a test message is noise.
		req.present["link_preview"] = true
		req.LinkPreview = false
	}

	if to != "" {
		req.present["to"] = true
		req.To = []string{to}
	}
	if ch.Type == config.TypeEmail {
		req.present["subject"] = true
		req.Subject = subject
	}
	return Validate(req, ch)
}

// testBody renders the same four facts in whichever markup was asked for.
//
// Telegram and email do not share one HTML body: Telegram's subset has no <br> and treats a
// newline as a line break, and email is the other way round.
func testBody(ch *config.Channel, bodyType string, now time.Time) string {
	stamp := now.Format(time.RFC3339)

	switch bodyType {
	case BodyHTML:
		if ch.Type == config.TypeTelegram {
			return "<b>notifio test</b>\n" +
				"channel <code>" + escapeHTML(ch.Name) + "</code> · " + ch.Type + "\n" +
				"sent " + stamp + " by notifio " + app.Version + "\n" +
				`<a href="` + app.ProjectURL + `">` + app.ProjectURL + "</a>"
		}
		return "<p><b>notifio test</b><br>\n" +
			"channel <code>" + escapeHTML(ch.Name) + "</code> &middot; " + ch.Type + "<br>\n" +
			"sent " + stamp + " by notifio " + app.Version + "</p>\n" +
			`<p><a href="` + app.ProjectURL + `">` + app.ProjectURL + "</a></p>"

	case BodyMD:
		// Escaped by hand, because notifio does not escape MarkdownV2 for anyone — including
		// itself. Getting this wrong is what the test message is for.
		return "*notifio test*\n" +
			"channel `" + ch.Name + "` — " + escapeMarkdownV2(ch.Type) + "\n" +
			"sent " + escapeMarkdownV2(stamp) + " by notifio " + escapeMarkdownV2(app.Version) + "\n" +
			"[" + escapeMarkdownV2(app.ProjectURL) + "](" + app.ProjectURL + ")"
	}

	return "notifio test\n" +
		"channel " + ch.Name + " · " + ch.Type + "\n" +
		"sent " + stamp + " by notifio " + app.Version + "\n" +
		app.ProjectURL
}

// markdownV2Special is every character MarkdownV2 reserves outside an entity.
const markdownV2Special = "_*[]()~`>#+-=|{}.!\\"

func escapeMarkdownV2(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(markdownV2Special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeHTML(s string) string { return htmlEscaper.Replace(s) }
