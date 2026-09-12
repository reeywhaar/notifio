// Package app holds what this program is, as distinct from what it was told.
package app

// Name is what this program is called, in a log line and on the command line.
const Name = "notifio"

// ProjectURL is where to read about it.
const ProjectURL = "https://github.com/reeywhaar/notifio"

// DefaultPort is where serve listens inside the container. PORT overrides it for development
// only; the image never sets it.
const DefaultPort = "80"

// Version is stamped at link time with -X notifio/internal/app.Version=…
var Version = "dev"
