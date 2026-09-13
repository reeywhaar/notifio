package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"notifio/internal/config"
	"notifio/internal/tokens"
)

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Mint, list and withdraw the tokens that may send",
	}
	cmd.AddCommand(tokenAddCmd(), tokenListCmd(), tokenRemoveCmd())
	return cmd
}

func tokenAddCmd() *cobra.Command {
	var nonced bool
	cmd := &cobra.Command{
		Use:   "add <label> <channel>",
		Short: "Mint a token for one channel and print it once",
		Long: "The secret is printed once and stored hashed. A lost token is removed and\n" +
			"minted again rather than recovered.\n\n" +
			"With --nonced the secret never crosses the wire: the caller sends a hash of it\n" +
			"with a timestamp, valid for five minutes. The cost is that notifio has to keep\n" +
			"the secret in data.json in the clear. See docs/tokens.md.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			label, channel := args[0], args[1]
			e, err := setup()
			if err != nil {
				return err
			}
			cfg, err := e.Config.Get()
			if err != nil {
				return err
			}
			// Checked at mint time so the only way to reach an orphaned token is to edit the
			// config afterwards.
			if _, ok := cfg.Channels[channel]; !ok {
				return fmt.Errorf("no channel %q; known channels are %v", channel, cfg.Names())
			}

			create := e.Tokens.Create
			if nonced {
				create = e.Tokens.CreateNonced
			}
			secret, err := create(label, channel)
			if err != nil {
				return err
			}
			// The secret alone on stdout, so $(…) captures exactly it.
			cmd.Println(secret)
			// The id goes to stderr with everything else, so $(…) still captures only the token.
			list, _ := e.Tokens.List()
			id := ""
			for _, t := range list {
				if t.Label == label {
					id = t.ID()
				}
			}
			kind := "bearer"
			if nonced {
				kind = "nonced"
			}
			fmt.Fprintf(os.Stderr, "notifio: minted %s token %q (id %s) for channel %q. It is not shown again.\n",
				kind, label, id, channel)
			if nonced {
				// The id is derivable from the secret, so a caller needs only the secret. Both
				// are printed because the recipe is easier to read with the real id in it.
				fmt.Fprintf(os.Stderr, "notifio: the caller needs only the secret; id is sha256(secret)[:8]\n")
				fmt.Fprintf(os.Stderr, "notifio: send  Authorization: Bearer %s<unix>.%s.<sha256 of \"<unix>.%s.<secret>\">\n",
					tokens.NoncedPrefix, id, id)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&nonced, "nonced", false, "the secret never crosses the wire; notifio stores it in the clear")
	return cmd
}

func tokenListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tokens: label, channel, type, created",
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
			list, err := e.Tokens.List()
			if err != nil {
				return err
			}

			type row struct {
				ID       string `json:"id"`
				Kind     string `json:"kind"`
				Label    string `json:"label"`
				Channel  string `json:"channel"`
				Type     string `json:"type"`
				Created  string `json:"created"`
				Orphaned bool   `json:"orphaned"`
			}
			rows := make([]row, 0, len(list))
			for _, t := range list {
				r := row{ID: t.ID(), Kind: "bearer", Label: t.Label, Channel: t.Channel,
					Created: t.Created().Format("2006-01-02 15:04:05Z")}
				if t.Nonced() {
					r.Kind = "nonced"
				}
				if ch, ok := cfg.Channels[t.Channel]; ok {
					r.Type = ch.Type
				} else {
					r.Type, r.Orphaned = "-", true
				}
				rows = append(rows, r)
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(os.Stderr, "notifio: no tokens yet. Mint one with: notifio token add <label> <channel>")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tKIND\tLABEL\tCHANNEL\tTYPE\tCREATED")
			for _, r := range rows {
				name := r.Channel
				if r.Orphaned {
					name += " (missing)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Kind, r.Label, name, r.Type, r.Created)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

func tokenRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <label>",
		Short: "Withdraw a token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := setup()
			if err != nil {
				return err
			}
			if err := e.Tokens.Remove(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "notifio: removed %q. The running server stops accepting it on its next request.\n", args[0])
			return nil
		},
	}
}

// describe is what a channel points at, with nothing secret in it.
func describe(ch *config.Channel) string {
	switch ch.Type {
	case config.TypeTelegram:
		if len(ch.Pinned.To) > 0 {
			return "chat " + ch.Pinned.To[0] + " (pinned)"
		}
		return "any chat its bot can reach"
	case config.TypeEmail:
		d := fmt.Sprintf("%s:%d %s", ch.Host, ch.Port, ch.Encryption)
		if ch.Pinned.From != "" {
			d += " from " + ch.Pinned.From
		}
		if len(ch.Pinned.To) > 0 {
			d += " to " + strings.Join(ch.Pinned.To, ", ")
		}
		if ch.Pinned.Any() {
			d += " (pinned)"
		}
		return d
	}
	return ""
}
