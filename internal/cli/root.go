// Package cli is notifio's command line: the server, and the few things an operator needs a
// shell for.
package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"notifio/internal/app"
	"notifio/internal/config"
	"notifio/internal/tokens"
)

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:   app.Name,
		Short: "Turn a POST into a notification",
		Long: "notifio takes a POST and sends it through the channel its token was issued\n" +
			"for — a Telegram bot or an SMTP relay.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Cobra's print helpers write to stderr unless an output is set, which would make
	// `TOKEN=$(notifio token add x alerts)` capture nothing at all.
	cmd.SetOut(os.Stdout)
	cmd.AddCommand(serveCmd(), tokenCmd(), channelCmd(), backupCmd(), healthcheckCmd(), versionCmd())
	return cmd
}

// Execute runs the command line and returns a process exit code.
func Execute() int {
	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, app.Name+":", err)
		return 1
	}
	return 0
}

// env is what every command needs and what version and healthcheck deliberately do not.
type env struct {
	Env    *config.Env
	Config *config.Store
	Tokens *tokens.Store
}

// setup loads the environment, proves the volume, then reads the config and the tokens. Every
// command that touches the volume fails here, in the same way, with the same message.
func setup() (*env, error) {
	e, err := config.LoadEnv()
	if err != nil {
		return nil, err
	}
	if err := config.RequireDataDir(e.DataDir); err != nil {
		return nil, err
	}
	cfg, err := config.Open(e.ConfigPath)
	if err != nil {
		return nil, err
	}
	st, err := tokens.Open(e.DataDir)
	if err != nil {
		return nil, err
	}
	return &env{Env: e, Config: cfg, Tokens: st}, nil
}

// logger is JSON on stdout: these lines are an access log and something will parse them.
func logger(e *config.Env) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: e.LogLevel}))
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version this binary was built from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println(app.Version)
			return nil
		},
	}
}
