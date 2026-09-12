package send

import (
	"fmt"
	"net/mail"
	"slices"
	"strings"

	"notifio/internal/config"
	"notifio/internal/markup"
)

// NoSubject stands in when an email request carries none.
const NoSubject = "No subject"

// Telegram's own limits. They belong here because they decide a 422 before a byte leaves.
const (
	TelegramMaxText    = 4096
	TelegramMaxCaption = 1024
	TelegramMaxAlbum   = 10
)

// Valid is the request after its channel's validator has run.
type Valid struct {
	Channel *config.Channel
	To      []string
	From    string
	Subject string
	Body    string
	// Type is the markup Body is in, after any conversion: only plain or html reaches a
	// sender.
	Type string
	// Requested is what the caller asked for, which is what gets logged.
	Requested string
	Atts      []Attachment

	// LinkPreview is whether Telegram renders a preview card. Default true, which is
	// Telegram's own.
	LinkPreview bool
}

// Validate checks a request against the type of the channel its token was issued for.
func Validate(req *Request, ch *config.Channel) (*Valid, error) {
	// Refused rather than ignored: the token already decided the channel, and a field that
	// does nothing teaches a caller it does something.
	if req.has("channel") {
		return nil, fmt.Errorf("%w: this token sends through %q; there is no \"channel\" field", ErrRequest, ch.Name)
	}
	if err := requireNonEmpty(req, "body", req.Body); err != nil {
		return nil, err
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		return nil, fmt.Errorf(`%w: "body" is empty`, ErrRequest)
	}

	switch ch.Type {
	case config.TypeTelegram:
		return validateTelegram(req, ch, body)
	case config.TypeEmail:
		return validateEmail(req, ch, body)
	}
	return nil, fmt.Errorf("%w: channel %q has type %q", ErrRequest, ch.Name, ch.Type)
}

func validateTelegram(req *Request, ch *config.Channel, body string) (*Valid, error) {
	// `from` still has nowhere to go on a telegram message, so it is still refused. `subject`
	// does: it becomes a title above the body, because a notification that arrives looking
	// slightly odd beats one that did not arrive.
	if req.has("from") {
		return nil, fmt.Errorf(`%w: a telegram channel has no "from"`, ErrRequest)
	}

	linkPreview := true
	switch {
	case ch.Pinned.LinkPreview != nil:
		if req.has("link_preview") && req.LinkPreview != *ch.Pinned.LinkPreview {
			return nil, fmt.Errorf("%w: channel %q pins %q to %t, and a request cannot change it",
				ErrRequest, ch.Name, "link_preview", *ch.Pinned.LinkPreview)
		}
		linkPreview = *ch.Pinned.LinkPreview
	case req.has("link_preview"):
		linkPreview = req.LinkPreview
	}

	to, err := resolve(req, ch, "to")
	if err != nil {
		return nil, err
	}
	if len(to) > 1 {
		return nil, fmt.Errorf(`%w: a telegram channel takes exactly one "to", got %d`, ErrRequest, len(to))
	}

	requested, err := resolveBodyType(req, BodyPlain, BodyMD, BodyHTML)
	if err != nil {
		return nil, err
	}
	bodyType := requested
	if requested == BodyMD {
		// Rendered here rather than in the sender, so `md` means one thing and the senders
		// only ever see plain or html.
		if body, err = markup.ToTelegramHTML(body); err != nil {
			return nil, fmt.Errorf("%w: body is not valid markdown: %v", ErrRequest, err)
		}
		bodyType = BodyHTML
	}

	if len(req.Atts) > TelegramMaxAlbum {
		return nil, fmt.Errorf("%w: telegram takes at most %d attachments, got %d", ErrLimit, TelegramMaxAlbum, len(req.Atts))
	}
	body = withTitle(req.Subject, body, bodyType)

	// Counted after the title is added, since that is what gets sent. Refused rather than
	// truncated: the part thrown away is the part most likely to matter. The caption limit is
	// not checked here — a body too long for one is sent as its own message instead.
	if n := len([]rune(body)); n > TelegramMaxText {
		return nil, fmt.Errorf("%w: telegram takes at most %d characters, got %d", ErrLimit, TelegramMaxText, n)
	}

	return &Valid{Channel: ch, To: to, Body: body, Type: bodyType, Requested: requested,
		Atts: req.Atts, LinkPreview: linkPreview}, nil
}

func validateEmail(req *Request, ch *config.Channel, body string) (*Valid, error) {
	// link_preview is accepted and ignored here rather than refused. A mail client shows a link
	// as written and never expands it into a preview card, so there is nothing for the setting
	// to do — and a sender that cannot know which kind of channel it is talking to should not
	// lose its message over a field that is simply irrelevant.

	to, err := resolve(req, ch, "to")
	if err != nil {
		return nil, err
	}
	addrs := make([]string, 0, len(to))
	seen := map[string]bool{}
	for _, raw := range to {
		a, err := parseAddress(raw, "to")
		if err != nil {
			return nil, err
		}
		if seen[a.Address] {
			continue
		}
		seen[a.Address] = true
		addrs = append(addrs, raw)
	}

	fromList, err := resolve(req, ch, "from")
	if err != nil {
		return nil, err
	}
	if len(fromList) > 1 {
		return nil, fmt.Errorf(`%w: "from" takes one address, got %d`, ErrRequest, len(fromList))
	}
	from := fromList[0]
	if _, err := parseAddress(from, "from"); err != nil {
		return nil, err
	}

	// A line break is still refused rather than defaulted: that is header injection, which is
	// a different thing from a caller who had nothing to put in the field.
	if err := noCRLF(req.Subject, "subject"); err != nil {
		return nil, err
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		// Every mail client shows something here anyway, and refusing the send would lose the
		// message over a missing header.
		subject = NoSubject
	}

	requested, err := resolveBodyType(req, BodyPlain, BodyMD, BodyHTML)
	if err != nil {
		return nil, err
	}
	bodyType := requested
	if requested == BodyMD {
		if body, err = markup.ToHTML(body); err != nil {
			return nil, fmt.Errorf("%w: body is not valid markdown: %v", ErrRequest, err)
		}
		bodyType = BodyHTML
	}

	return &Valid{Channel: ch, To: addrs, From: from, Subject: subject, Body: body,
		Type: bodyType, Requested: requested, Atts: req.Atts}, nil
}

// withTitle puts a subject above a telegram body, in whatever markup the body is in.
//
// Telegram has no subject of its own, and the alternative to this was refusing the request —
// which loses the notification over a field the caller had no way to know was unwanted.
//
// The title is escaped because notifio is authoring this markup rather than relaying the
// caller's. It is applied after any markdown conversion, so it only ever has to speak plain
// or HTML.
func withTitle(subject, body, bodyType string) string {
	title := collapseSpace(subject)
	if title == "" {
		return body
	}
	if bodyType == BodyHTML {
		return "<b>" + markup.EscapeHTML(title) + "</b>\n\n" + body
	}
	return title + "\n\n" + body
}

// collapseSpace flattens a subject to one line. A title that wrapped would not read as one.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// resolve applies the rule every field in [config.Pinned] follows: what the config pins, the
// config decides.
//
// A request may *state* a pinned value and may not *contradict* one. Saying the same thing is
// not a disagreement, and refusing it would break a caller that documents its own destination —
// but a value that differs is refused rather than quietly overridden, because a caller told
// their message went to one place while it went to another has been lied to with a 200.
func resolve(req *Request, ch *config.Channel, field string) ([]string, error) {
	var pin, given []string
	switch field {
	case "to":
		pin, given = ch.Pinned.To, req.To
	case "from":
		pin = nonEmpty(ch.Pinned.From)
		given = nonEmpty(req.From)
	}

	if len(pin) > 0 {
		if req.has(field) && !sameValues(pin, given) {
			return nil, fmt.Errorf("%w: channel %q pins %q to %s, and a request cannot change it",
				ErrRequest, ch.Name, field, strings.Join(pin, ", "))
		}
		return pin, nil
	}

	if !req.has(field) {
		return nil, fmt.Errorf("%w: %q is required; channel %q pins none", ErrRequest, field, ch.Name)
	}
	if len(given) == 0 {
		return nil, fmt.Errorf("%w: %q was given with no value", ErrRequest, field)
	}
	for _, v := range given {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("%w: %q was given empty", ErrRequest, field)
		}
	}
	return given, nil
}

// sameValues compares what was pinned against what was sent, as sets of trimmed strings.
//
// Textual, deliberately: "Notifio <n@x.com>" and "n@x.com" name the same mailbox and are not
// the same value, and a rule that sometimes looked through a display name would be one nobody
// could predict. State what is pinned.
func sameValues(pin, given []string) bool {
	if len(pin) != len(given) {
		return false
	}
	left := make([]string, len(pin))
	right := make([]string, len(given))
	for i := range pin {
		left[i] = strings.TrimSpace(pin[i])
	}
	for i := range given {
		right[i] = strings.TrimSpace(given[i])
	}
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

func nonEmpty(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return []string{v}
}

func resolveBodyType(req *Request, allowed ...string) (string, error) {
	if !req.has("body_type") {
		return allowed[0], nil
	}
	v := strings.TrimSpace(req.BodyType)
	if v == "" {
		return "", fmt.Errorf(`%w: "body_type" was given empty`, ErrRequest)
	}
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	return "", fmt.Errorf("%w: body_type %q is not one of %s", ErrRequest, v, strings.Join(allowed, ", "))
}

func requireNonEmpty(req *Request, field, value string) error {
	if !req.has(field) {
		return fmt.Errorf("%w: %q is required", ErrRequest, field)
	}
	if value == "" {
		return fmt.Errorf("%w: %q was given empty", ErrRequest, field)
	}
	return nil
}

// parseAddress refuses a comma-separated list. "Doe, Jane" <jane@x> is one valid address with
// a comma in it, so splitting on commas is wrong for exactly the names written properly.
func parseAddress(raw, field string) (*mail.Address, error) {
	if err := noCRLF(raw, field); err != nil {
		return nil, err
	}
	a, err := mail.ParseAddress(raw)
	if err != nil {
		if list, lerr := mail.ParseAddressList(raw); lerr == nil && len(list) > 1 {
			return nil, fmt.Errorf("%w: %q holds %d addresses; repeat %q instead", ErrRequest, field, len(list), field)
		}
		return nil, fmt.Errorf("%w: %s %q: %v", ErrRequest, field, raw, err)
	}
	return a, nil
}

// noCRLF is the header-injection refusal. It is explicit rather than left to the encoder,
// because this is the input that turns a notifier into an open relay.
func noCRLF(v, field string) error {
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("%w: %s contains a line break", ErrRequest, field)
	}
	return nil
}
