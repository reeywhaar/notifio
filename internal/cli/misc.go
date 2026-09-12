package cli

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"notifio/internal/app"
	"notifio/internal/config"
)

// healthcheckCmd is what the image's HEALTHCHECK runs, so the container needs no HTTP client.
//
// It does not call setup(): a healthcheck that read the volume would report the volume rather
// than the server, and the server is what is being asked about.
func healthcheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "healthcheck",
		Short: "Ask the running server whether it is well",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := config.LoadEnv()
			if err != nil {
				return err
			}
			port := e.Port
			if port == "" {
				port = app.DefaultPort
			}
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("healthz answered %s", resp.Status)
			}
			return nil
		},
	}
}
