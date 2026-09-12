package send

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"notifio/internal/app"
	"notifio/internal/channel"
	"notifio/internal/config"
	"notifio/internal/tokens"
)

// Error codes, as they appear in a failure body.
const (
	codeRequest  = "request"
	codeAuth     = "auth"
	codeLimit    = "limit"
	codeConfig   = "config"
	codeProvider = "provider"
)

// Handler answers /api/send and /healthz.
type Handler struct {
	Config *config.Store
	Tokens *tokens.Store
	Log    *slog.Logger
	Client *http.Client

	mu         sync.Mutex
	lastCfgErr string
}

// config is the config in use, and the one place a load failure is reported.
//
// Logged on the edge rather than on every call: the healthcheck asks every thirty seconds, and
// a broken file would otherwise fill the log with the same line until somebody fixed it.
func (h *Handler) config() (*config.Config, bool) {
	cfg, err := h.Config.Get()
	msg := ""
	if err != nil {
		msg = err.Error()
	}

	h.mu.Lock()
	changed := msg != h.lastCfgErr
	h.lastCfgErr = msg
	h.mu.Unlock()

	switch {
	case changed && msg != "":
		h.Log.Error("config will not load; still serving the last good one", "error", msg)
	case changed && msg == "":
		h.Log.Info("config loaded again", "channels", cfg.Names())
	}
	return cfg, err == nil
}

// Routes returns the mux. Two exact paths and a catch-all.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/send", h.send)
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeRequest, "no such path")
	})
	return mux
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, codeRequest, "healthz takes GET")
		return
	}
	cfg, ok := h.config()
	body := map[string]any{"ok": true, "version": app.Version, "channels": len(cfg.Channels)}
	if !ok {
		// Still 200, and deliberately: a 503 here would fail the container healthcheck, and
		// the restart that follows would find the same broken file and refuse to start at
		// all — turning a notifier that is still sending into one that is not.
		body["config"] = "stale"
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, codeRequest, "/api/send takes POST")
		return
	}

	// Auth first, and from the headers alone: a token in the body could not be checked until
	// the body was read, which would let a stranger make notifio buffer a 25 MB upload.
	tok, ok, err := h.Tokens.Verify(bearer(r))
	if err != nil {
		h.Log.Error("token file unreadable", "error", err)
		writeError(w, http.StatusInternalServerError, codeConfig, "the token file could not be read")
		return
	}
	if !ok {
		h.Log.Warn("rejected", "reason", "unknown token", "client", clientIP(r))
		writeError(w, http.StatusUnauthorized, codeAuth, "unknown token")
		return
	}

	cfg, _ := h.config()
	ch, known := cfg.Channels[tok.Channel]
	if !known {
		// Not 401: the credential is fine and hunting for a revoked one is an hour lost.
		h.Log.Error("orphaned token", "token", tok.Label, "channel", tok.Channel)
		writeError(w, http.StatusServiceUnavailable, codeConfig,
			"channel "+tok.Channel+", which this token was issued for, is no longer configured")
		return
	}

	// The channel is known before the body is touched, so the cap is that channel's own.
	r.Body = http.MaxBytesReader(w, r.Body, int64(ch.MaxBody))

	req, err := Parse(r, int64(ch.MaxBody))
	if req != nil {
		defer req.Cleanup()
	}
	if err != nil {
		h.fail(w, r, tok, ch, started, err)
		return
	}

	v, err := Validate(req, ch)
	if err != nil {
		h.fail(w, r, tok, ch, started, err)
		return
	}

	res, err := Deliver(r.Context(), v, h.Client)
	if err != nil {
		h.failSend(w, r, tok, ch, v, started, res, err)
		return
	}

	h.Log.Info("sent",
		"token", tok.Label, "channel", ch.Name, "type", ch.Type,
		"to", redactTo(ch.Type, v.To), "body_type", v.Requested, "attachments", len(v.Atts),
		"status", http.StatusOK, "dur_ms", ms(started), "id", res.ID, "client", clientIP(r))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "channel": ch.Name, "type": ch.Type, "id": res.ID, "dur_ms": ms(started),
	})
}

// fail maps a parse or validation error onto a status.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, tok tokens.Token, ch *config.Channel, started time.Time, err error) {
	status, code := http.StatusBadRequest, codeRequest
	switch {
	case errors.Is(err, ErrTooLarge), isMaxBytes(err):
		status, code = http.StatusRequestEntityTooLarge, codeRequest
	case errors.Is(err, ErrUnsupportedMedia):
		status, code = http.StatusUnsupportedMediaType, codeRequest
	case errors.Is(err, ErrLimit):
		status, code = http.StatusUnprocessableEntity, codeLimit
	}
	h.Log.Warn("refused",
		"token", tok.Label, "channel", ch.Name, "type", ch.Type,
		"status", status, "dur_ms", ms(started), "error", err.Error(), "client", clientIP(r))
	writeError(w, status, code, message(err))
}

// failSend maps a provider error. A 502 rather than a 400: notifio's caller did nothing wrong,
// and telling an alerting script to stop retrying is the wrong answer.
func (h *Handler) failSend(w http.ResponseWriter, r *http.Request, tok tokens.Token, ch *config.Channel, v *Valid, started time.Time, res *channel.Result, err error) {
	status, code := http.StatusBadGateway, codeProvider
	if errors.Is(err, channel.ErrUnreachable) {
		status = http.StatusBadGateway
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") {
		status = http.StatusGatewayTimeout
	}

	h.Log.Error("send failed",
		"token", tok.Label, "channel", ch.Name, "type", ch.Type,
		"to", redactTo(ch.Type, v.To), "status", status, "dur_ms", ms(started),
		"error", err.Error(), "client", clientIP(r))

	body := map[string]any{"ok": false, "code": code, "error": message(err)}
	if res != nil && res.Partial {
		// The one case that cannot be atomic says so rather than being rounded either way.
		body["partial"] = true
	}
	writeJSON(w, status, body)
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(v, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// isMaxBytes recognises what MaxBytesReader returns, which is not one of our errors.
func isMaxBytes(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe) || strings.Contains(err.Error(), "request body too large")
}

// message strips the wrapping sentinel, which names a category and not a cause.
func message(err error) string {
	s := err.Error()
	for _, p := range []string{"request: ", "limit: ", "refused: ", "unreachable: ", "unsupported media type: "} {
		s = strings.TrimPrefix(s, p)
	}
	return s
}

// redactTo is enough to tell two destinations apart in an incident and not enough to be a
// recipient list. The full value is at debug, which is a deliberate opt-in.
func redactTo(typ string, to []string) string {
	if len(to) == 0 {
		return ""
	}
	switch typ {
	case config.TypeTelegram:
		v := to[0]
		if len(v) > 4 {
			return "…" + v[len(v)-4:]
		}
		return v
	case config.TypeEmail:
		out := make([]string, 0, len(to))
		for _, a := range to {
			if i := strings.LastIndex(a, "@"); i >= 0 {
				out = append(out, "…@"+strings.TrimSuffix(a[i+1:], ">"))
			} else {
				out = append(out, "…")
			}
		}
		return strings.Join(out, ",")
	}
	return "…"
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func ms(started time.Time) int64 { return time.Since(started).Milliseconds() }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "code": code, "error": msg})
}
