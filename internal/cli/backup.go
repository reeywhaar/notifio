package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"notifio/internal/backup"
	"notifio/internal/config"
)

func backupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Push a copy of config.json and data.json to a backup agent",
	}
	cmd.AddCommand(backupNowCmd())
	return cmd
}

func backupNowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "now",
		Short: "Push one archive, whether or not anything changed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := setup()
			if err != nil {
				return err
			}
			if e.Env.BackupURL == "" {
				return fmt.Errorf("%s is not set; there is nowhere to send a copy", config.BackupURLEnv)
			}
			p := &backup.Pusher{
				URL:        e.Env.BackupURL,
				ConfigPath: e.Config.Path(),
				DataPath:   e.Tokens.Path(),
				Log:        logger(e.Env),
			}
			if _, err := p.Once(cmd.Context(), true); err != nil {
				return err
			}
			cmd.Println("sent")
			return nil
		},
	}
}
