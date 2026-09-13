package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"notifio/internal/app"
	"notifio/internal/backup"
	"notifio/internal/config"
	"notifio/internal/send"
)

// shutdownGrace matches Docker's default stop timeout; after it, SIGKILL is the backstop.
const shutdownGrace = 10 * time.Second

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Answer POST /api/send",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := setup()
			if err != nil {
				return err
			}
			log := logger(e.Env)
			// Only here, not in setup(): logger writes to stdout, and a stray line there would
			// corrupt `notifio token list --json`.
			e.Tokens.Log = log

			cfg, err := e.Config.Get()
			if err != nil {
				return err
			}
			// A warning rather than a refusal: crash-looping would stop anyone getting a
			// shell to mint the first token.
			if n, err := e.Tokens.Count(); err == nil && n == 0 {
				log.Warn("no tokens exist; nothing can send",
					"fix", "notifio token add <label> <channel>", "channels", cfg.Names())
			}

			port := e.Env.Port
			if port == "" {
				port = app.DefaultPort
			}

			h := &send.Handler{
				Config: e.Config,
				Tokens: e.Tokens,
				Log:    log,
				Client: &http.Client{},
			}
			srv := &http.Server{
				Addr:    ":" + port,
				Handler: h.Routes(),
				// WriteTimeout must outlast the longest send_timeout, or the server cuts the
				// response off before the send's own timeout can produce a 504.
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       120 * time.Second,
				WriteTimeout:      longestSendTimeout(cfg) + 30*time.Second,
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			if e.Env.BackupURL != "" {
				p := &backup.Pusher{
					URL:        e.Env.BackupURL,
					ConfigPath: e.Config.Path(),
					DataPath:   e.Tokens.Path(),
					Log:        log,
				}
				go p.Run(ctx)
			}

			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return err
			}
			log.Info("listening", "addr", srv.Addr, "version", app.Version, "channels", cfg.Names())

			errc := make(chan error, 1)
			go func() { errc <- srv.Serve(ln) }()

			select {
			case err := <-errc:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			case <-ctx.Done():
			}

			log.Info("shutting down", "grace", shutdownGrace.String())
			sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			return srv.Shutdown(sctx)
		},
	}
}

func longestSendTimeout(cfg *config.Config) time.Duration {
	var longest time.Duration
	for _, ch := range cfg.Channels {
		if d := ch.SendTimeout.D(); d > longest {
			longest = d
		}
	}
	return longest
}
