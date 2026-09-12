// Command notifio turns a POST into a notification.
package main

import (
	"os"
	"time"

	"notifio/internal/cli"
)

func main() {
	// The image carries no zone database, so a named TZ would resolve to UTC anyway.
	// Pinning it makes that a decision rather than a side effect.
	time.Local = time.UTC
	os.Exit(cli.Execute())
}
