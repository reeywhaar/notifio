package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"notifio/internal/send"
)

func channelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channel",
		Short: "Inspect and test the configured channels",
	}
	cmd.AddCommand(channelListCmd(), channelTestCmd())
	return cmd
}

func channelListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List channels: name, type, destination. Never a credential",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := setup()
			if err != nil {
				return err
			}
			cfg, err := e.Config.Get()
			if err != nil {
				return err
			}

			type row struct {
				Name        string `json:"name"`
				Type        string `json:"type"`
				Destination string `json:"destination"`
				MaxBody     string `json:"max_body"`
				SendTimeout string `json:"send_timeout"`
			}
			rows := make([]row, 0, len(cfg.Channels))
			for _, name := range cfg.Names() {
				ch := cfg.Channels[name]
				rows = append(rows, row{
					Name: name, Type: ch.Type, Destination: describe(ch),
					MaxBody: ch.MaxBody.String(), SendTimeout: ch.SendTimeout.String(),
				})
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTYPE\tDESTINATION\tMAX BODY\tSEND TIMEOUT")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Type, r.Destination, r.MaxBody, r.SendTimeout)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

func channelTestCmd() *cobra.Command {
	var to, subject, bodyType string
	cmd := &cobra.Command{
		Use:   "test <name>",
		Short: "Send a real message through a channel and print what came back",
		Long: "Sends a marked-up message by default, so the round trip exercises parse_mode\n" +
			"or the HTML part rather than only the connection. Use --body-type to pick\n" +
			"another: plain, html, or md on a telegram channel.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := setup()
			if err != nil {
				return err
			}
			cfg, err := e.Config.Get()
			if err != nil {
				return err
			}
			ch, ok := cfg.Channels[args[0]]
			if !ok {
				return fmt.Errorf("no channel %q; known channels are %v", args[0], cfg.Names())
			}

			v, err := send.TestMessage(ch, to, subject, bodyType)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "notifio: sending through %q...\n", ch.Name)

			res, err := send.Deliver(cmd.Context(), v, &http.Client{})
			if err != nil {
				return err
			}
			cmd.Printf("sent, id %s\n", res.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "where to send it; required on an open channel, refused on a pinned one")
	cmd.Flags().StringVar(&subject, "subject", "notifio test", "subject, on an email channel")
	cmd.Flags().StringVar(&bodyType, "body-type", send.BodyHTML, "plain, html, or md on a telegram channel")
	return cmd
}
