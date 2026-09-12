// Package mail sends through an SMTP relay.
package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"time"

	"notifio/internal/channel"
	"notifio/internal/config"
)

// Sender is one configured email channel.
type Sender struct {
	cfg *config.Channel
}

// New returns a sender for an email channel.
func New(cfg *config.Channel) *Sender { return &Sender{cfg: cfg} }

// Send delivers one message. Any rejected recipient fails the whole send: a caller that got a
// 200 must never have to wonder who received it.
func (s *Sender) Send(ctx context.Context, msg *channel.Message) (*channel.Result, error) {
	from, rcpt, err := Envelope(msg.From, msg.To)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", channel.ErrRefused, err)
	}
	id, err := NewID(msg.From)
	if err != nil {
		return nil, err
	}

	c, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	if err := s.auth(c); err != nil {
		return nil, err
	}
	if err := c.Mail(from); err != nil {
		return nil, fmt.Errorf("%w: MAIL FROM %s: %v", channel.ErrRefused, from, err)
	}
	for _, r := range rcpt {
		if err := c.Rcpt(r); err != nil {
			return nil, fmt.Errorf("%w: RCPT TO %s: %v", channel.ErrRefused, r, err)
		}
	}

	w, err := c.Data()
	if err != nil {
		return nil, fmt.Errorf("%w: DATA: %v", channel.ErrRefused, err)
	}
	m := &Message{
		From: msg.From, To: msg.To, Subject: msg.Subject,
		Body: msg.Body, BodyType: msg.BodyType, Atts: msg.Atts, ID: id,
	}
	if err := m.Render(w); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("%w: %v", channel.ErrRefused, err)
	}
	_ = c.Quit()
	return &channel.Result{ID: id}, nil
}

// dial opens the connection the channel's encryption asks for.
func (s *Sender) dial(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	d := &net.Dialer{}

	if s.cfg.Encryption == config.EncTLS {
		conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: s.cfg.Host})
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", channel.ErrUnreachable, addr, err)
		}
		c, err := smtp.NewClient(conn, s.cfg.Host)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("%w: %s: %v", channel.ErrUnreachable, addr, err)
		}
		return c, nil
	}

	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", channel.ErrUnreachable, addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(s.cfg.SendTimeout.D()))
	}
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: %s: %v", channel.ErrUnreachable, addr, err)
	}

	if s.cfg.Encryption == config.EncStartTLS {
		// Load-bearing: without this check a relay that has lost its certificate downgrades
		// to plaintext and the credentials go out in the clear, silently.
		if ok, _ := c.Extension("STARTTLS"); !ok {
			c.Close()
			return nil, fmt.Errorf("%w: %s does not offer STARTTLS", channel.ErrRefused, addr)
		}
		if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
			c.Close()
			return nil, fmt.Errorf("%w: STARTTLS: %v", channel.ErrRefused, err)
		}
	}
	return c, nil
}

func (s *Sender) auth(c *smtp.Client) error {
	if s.cfg.User == "" && s.cfg.Password == "" {
		return nil
	}
	if ok, _ := c.Extension("AUTH"); !ok {
		return fmt.Errorf("%w: the relay offers no AUTH", channel.ErrRefused)
	}
	if err := c.Auth(smtp.PlainAuth("", s.cfg.User, s.cfg.Password, s.cfg.Host)); err != nil {
		return fmt.Errorf("%w: AUTH: %v", channel.ErrRefused, err)
	}
	return nil
}
