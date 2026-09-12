// Package telegram sends through a Telegram bot.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"notifio/internal/channel"
	"notifio/internal/config"
)

// Telegram's limits. maxCaption is the one that shapes the code: a body over it cannot ride
// with the files and goes as its own message first.
const (
	maxCaption = 1024
	maxAlbum   = 10
)

// Sender is one configured telegram channel.
type Sender struct {
	cfg    *config.Channel
	client *http.Client
}

// New returns a sender for a telegram channel.
func New(cfg *config.Channel, client *http.Client) *Sender {
	if client == nil {
		client = http.DefaultClient
	}
	return &Sender{cfg: cfg, client: client}
}

// Send delivers one message, choosing the method by attachment count.
func (s *Sender) Send(ctx context.Context, msg *channel.Message) (*channel.Result, error) {
	if len(msg.Atts) > maxAlbum {
		return nil, fmt.Errorf("%w: at most %d attachments", channel.ErrRefused, maxAlbum)
	}
	chat := ""
	if len(msg.To) > 0 {
		chat = msg.To[0]
	}

	caption := msg.Body
	var textFirst bool
	if len(msg.Atts) > 0 && len([]rune(msg.Body)) > maxCaption {
		// Two calls, text first, so it arrives above its attachments. Truncating to the
		// caption limit would throw away the part most likely to matter.
		textFirst = true
		caption = ""
	}

	var firstID string
	if textFirst || len(msg.Atts) == 0 {
		id, err := s.sendMessage(ctx, chat, msg.Body, msg.BodyType, msg.LinkPreview)
		if err != nil {
			return nil, err
		}
		firstID = id
		if len(msg.Atts) == 0 {
			return &channel.Result{ID: id}, nil
		}
	}

	id, err := s.sendFiles(ctx, chat, caption, msg.BodyType, msg.Atts)
	if err != nil {
		// The text is already delivered and the files are not. This is the one path that
		// cannot be atomic, and it says so rather than being rounded to either answer.
		return &channel.Result{ID: firstID, Partial: textFirst}, err
	}
	if firstID == "" {
		firstID = id
	}
	return &channel.Result{ID: firstID}, nil
}

func (s *Sender) sendMessage(ctx context.Context, chat, text, bodyType string, linkPreview bool) (string, error) {
	form := url.Values{}
	form.Set("chat_id", chat)
	form.Set("text", text)
	if mode := parseMode(bodyType); mode != "" {
		form.Set("parse_mode", mode)
	}
	if !linkPreview {
		// link_preview_options rather than the deprecated disable_web_page_preview.
		form.Set("link_preview_options", `{"is_disabled":true}`)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.method("sendMessage"), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return s.do(req, false)
}

func (s *Sender) sendFiles(ctx context.Context, chat, caption, bodyType string, atts []channel.Attachment) (string, error) {
	method := "sendDocument"
	if len(atts) > 1 {
		method = "sendMediaGroup"
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(s.writeFiles(mw, method, chat, caption, bodyType, atts))
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.method(method), pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return s.do(req, method == "sendMediaGroup")
}

func (s *Sender) writeFiles(mw *multipart.Writer, method, chat, caption, bodyType string, atts []channel.Attachment) error {
	defer mw.Close()

	if err := mw.WriteField("chat_id", chat); err != nil {
		return err
	}

	if method == "sendDocument" {
		if caption != "" {
			if err := mw.WriteField("caption", caption); err != nil {
				return err
			}
			if mode := parseMode(bodyType); mode != "" {
				if err := mw.WriteField("parse_mode", mode); err != nil {
					return err
				}
			}
		}
		return copyPart(mw, "document", atts[0])
	}

	// An album refers to its files by name: each media entry carries attach://<field>, and the
	// file part is named <field>. Everything is sent as a document, which avoids Telegram's
	// rules about which media types may share an album.
	media := make([]map[string]string, 0, len(atts))
	for i := range atts {
		field := "file" + strconv.Itoa(i)
		m := map[string]string{"type": "document", "media": "attach://" + field}
		if i == 0 && caption != "" {
			m["caption"] = caption
			if mode := parseMode(bodyType); mode != "" {
				m["parse_mode"] = mode
			}
		}
		media = append(media, m)
	}
	spec, err := json.Marshal(media)
	if err != nil {
		return err
	}
	if err := mw.WriteField("media", string(spec)); err != nil {
		return err
	}
	for i, a := range atts {
		if err := copyPart(mw, "file"+strconv.Itoa(i), a); err != nil {
			return err
		}
	}
	return nil
}

func copyPart(mw *multipart.Writer, field string, a channel.Attachment) error {
	part, err := mw.CreateFormFile(field, a.Filename)
	if err != nil {
		return err
	}
	rc, err := a.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(part, rc)
	return err
}

// reply is the envelope every Bot API method answers with.
type reply struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (s *Sender) do(req *http.Request, album bool) (string, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", channel.ErrUnreachable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", fmt.Errorf("%w: %v", channel.ErrUnreachable, err)
	}
	var r reply
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("%w: telegram answered %s with %q", channel.ErrRefused, resp.Status, truncate(string(body), 200))
	}
	if !r.OK {
		// Relayed verbatim: the description is the only thing that says what to fix. Waiting
		// out a retry_after here would hold a caller's request open through an incident.
		if r.Parameters.RetryAfter > 0 {
			return "", fmt.Errorf("%w: %s (retry after %ds)", channel.ErrRefused, r.Description, r.Parameters.RetryAfter)
		}
		return "", fmt.Errorf("%w: %s", channel.ErrRefused, r.Description)
	}
	return messageID(r.Result, album), nil
}

func messageID(result json.RawMessage, album bool) string {
	if album {
		var many []struct {
			MessageID int64 `json:"message_id"`
		}
		if err := json.Unmarshal(result, &many); err == nil && len(many) > 0 {
			return strconv.FormatInt(many[0].MessageID, 10)
		}
		return ""
	}
	var one struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(result, &one); err == nil {
		return strconv.FormatInt(one.MessageID, 10)
	}
	return ""
}

func (s *Sender) method(name string) string {
	return strings.TrimSuffix(s.cfg.APIBase, "/") + "/bot" + s.cfg.Token + "/" + name
}

// parseMode maps notifio's body types onto Telegram's. plain has none.
func parseMode(bodyType string) string {
	switch bodyType {
	case "md":
		return "MarkdownV2"
	case "html":
		return "HTML"
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
