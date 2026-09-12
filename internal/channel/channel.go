// Package channel is what a configured destination can do.
package channel

import (
	"context"
	"errors"
	"io"
)

// ErrRefused is the provider saying no. ErrUnreachable is it not answering. The handler picks
// 502 from 504 on these rather than by matching on strings.
var (
	ErrRefused     = errors.New("refused")
	ErrUnreachable = errors.New("unreachable")
)

// Attachment is one file, as a sender needs it.
type Attachment struct {
	Filename    string
	ContentType string
	Size        int64
	Open        func() (io.ReadCloser, error)
}

// Message is what a validated request becomes.
type Message struct {
	To       []string
	From     string
	Subject  string
	Body     string
	BodyType string
	Atts     []Attachment

	// LinkPreview is whether Telegram renders a preview card for a link in the body.
	LinkPreview bool
}

// Result is what came back.
type Result struct {
	// ID is the provider's own identifier, for quoting when asking about a message.
	ID string
	// Partial is true when the body and its attachments went as separate calls and the second
	// failed. It exists for the one path that cannot be atomic.
	Partial bool
}

// Sender is one channel type.
type Sender interface {
	Send(ctx context.Context, msg *Message) (*Result, error)
}
