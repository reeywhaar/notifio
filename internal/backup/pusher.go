package backup

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// interval is how often the two files are checked, and the throttle as much as the delay: four
// tokens minted in a minute produce one archive holding all four.
const interval = 5 * time.Minute

// pushTimeout bounds one pass, end to end.
const pushTimeout = 10 * time.Minute

// Pusher copies the two files to a backup agent when either has changed.
type Pusher struct {
	URL        string
	ConfigPath string
	DataPath   string
	Log        *slog.Logger

	// Every is how often to look, and defaults to [interval]. A field only so a test can drive
	// the loop without waiting five minutes.
	Every time.Duration

	Client *http.Client

	lastDigest string
}

// Run works until the context is done.
//
// The first pass is immediate: a process that has just started is the one most likely to be
// running on a new volume, and it is also how a misconfigured agent reports itself within
// seconds of boot rather than on the day it is needed.
func (p *Pusher) Run(ctx context.Context) {
	if p.URL == "" {
		return
	}
	if p.Every <= 0 {
		p.Every = interval
	}
	p.Log.Info("backing up", "to", p.URL, "every", p.Every.String())

	for {
		if _, err := p.Once(ctx, false); err != nil {
			// Logged and left for the next pass. What fails is either transient or needs a
			// person, and neither is helped by trying again a second later.
			p.Log.Error("backup failed", "error", err)
		}
		timer := time.NewTimer(p.Every)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Once builds an archive and pushes it. Unless force is set, an unchanged pair sends nothing.
// It returns the name of the archive that went, or empty when nothing did.
func (p *Pusher) Once(ctx context.Context, force bool) (string, error) {
	now := time.Now()
	a, err := Build([]string{p.ConfigPath, p.DataPath}, now)
	if err != nil {
		return "", err
	}
	// The digest is over the contents, not the mtimes: a restart that rewrites nothing must
	// not produce an archive, and neither must an editor that saves identical bytes.
	if !force && a.Digest == p.lastDigest {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	client := p.Client
	if client == nil {
		client = &http.Client{}
	}
	if err := Push(ctx, client, p.URL, Name, a.Body); err != nil {
		return "", err
	}
	p.lastDigest = a.Digest
	p.Log.Info("backed up", "bytes", len(a.Body))
	return Name, nil
}
