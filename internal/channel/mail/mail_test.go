package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"notifio/internal/channel"
	"notifio/internal/config"
)

// fakeSMTP is a relay that says yes to everything except the recipients in refuse.
type fakeSMTP struct {
	ln          net.Listener
	offerTLS    bool
	offerAuth   bool
	refuseRcpt  map[string]bool
	mu          sync.Mutex
	data        string
	rcptSeen    []string
	sawStartTLS bool
}

func newFakeSMTP(t *testing.T, offerTLS, offerAuth bool, refuse ...string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, offerTLS: offerTLS, offerAuth: offerAuth, refuseRcpt: map[string]bool{}}
	for _, r := range refuse {
		f.refuseRcpt[r] = true
	}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) addr() (string, int) {
	h, p, _ := net.SplitHostPort(f.ln.Addr().String())
	port := 0
	for _, c := range p {
		port = port*10 + int(c-'0')
	}
	return h, port
}

func (f *fakeSMTP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeSMTP) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(s string) { conn.Write([]byte(s + "\r\n")) }

	w("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			ext := []string{"250-fake"}
			if f.offerTLS {
				ext = append(ext, "250-STARTTLS")
			}
			if f.offerAuth {
				ext = append(ext, "250-AUTH PLAIN")
			}
			ext = append(ext, "250 8BITMIME")
			w(strings.Join(ext, "\r\n"))
		case strings.HasPrefix(cmd, "STARTTLS"):
			f.mu.Lock()
			f.sawStartTLS = true
			f.mu.Unlock()
			// Answered but never completed: the test only needs to know it was reached.
			w("454 TLS not available here")
		case strings.HasPrefix(cmd, "AUTH"):
			w("235 ok")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			w("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			addr := strings.Trim(strings.TrimPrefix(cmd, "RCPT TO:"), "<> ")
			f.mu.Lock()
			f.rcptSeen = append(f.rcptSeen, addr)
			refused := f.refuseRcpt[strings.ToLower(addr)]
			f.mu.Unlock()
			if refused {
				w("550 no such user")
			} else {
				w("250 ok")
			}
		case cmd == "DATA":
			w("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			w("250 queued")
		case cmd == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func (f *fakeSMTP) received() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func chanFor(f *fakeSMTP, enc string) *config.Channel {
	host, port := f.addr()
	return &config.Channel{
		Name: "notices", Type: config.TypeEmail, Host: host, Port: port, Encryption: enc,
		MaxBody: config.DefaultMaxBody, SendTimeout: config.Duration(config.DefaultSendTimeout),
	}
}

func msg() *channel.Message {
	return &channel.Message{
		From: "Notifio <n@example.com>", To: []string{"a@x.com"},
		Subject: "Disk at 91%", Body: "it is full", BodyType: "plain",
	}
}

func TestSendDelivers(t *testing.T) {
	f := newFakeSMTP(t, false, false)
	res, err := New(chanFor(f, config.EncNone)).Send(context.Background(), msg())
	if err != nil {
		t.Fatal(err)
	}
	if res.ID == "" {
		t.Error("no Message-ID came back")
	}
	got := f.received()
	if !strings.Contains(got, "Subject:") || !strings.Contains(got, "it is full") {
		t.Errorf("the relay received:\n%s", got)
	}
	if !strings.Contains(got, "<"+res.ID+">") {
		t.Error("the returned id is not the message's own Message-ID")
	}
}

// Without this check a relay that has lost its certificate downgrades to plaintext and the
// credentials go out in the clear, silently.
func TestStartTLSMustBeOffered(t *testing.T) {
	f := newFakeSMTP(t, false, true)
	ch := chanFor(f, config.EncStartTLS)
	ch.User, ch.Password = "u", "p"

	_, err := New(ch).Send(context.Background(), msg())
	if err == nil {
		t.Fatal("a relay with no STARTTLS was used anyway")
	}
	if !errors.Is(err, channel.ErrRefused) || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("err = %v", err)
	}
	if f.received() != "" {
		t.Error("something was delivered over the plaintext connection")
	}
}

func TestStartTLSIsAttemptedWhenOffered(t *testing.T) {
	f := newFakeSMTP(t, true, false)
	if _, err := New(chanFor(f, config.EncStartTLS)).Send(context.Background(), msg()); err == nil {
		t.Fatal("the failed TLS upgrade was ignored")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.sawStartTLS {
		t.Error("STARTTLS was never sent")
	}
}

// A caller that got a 200 must never have to wonder who received it.
func TestOneRejectedRecipientAbortsTheSend(t *testing.T) {
	f := newFakeSMTP(t, false, false, "gone@x.com")
	m := msg()
	m.To = []string{"a@x.com", "gone@x.com", "b@x.com"}

	_, err := New(chanFor(f, config.EncNone)).Send(context.Background(), m)
	if err == nil {
		t.Fatal("a rejected recipient did not fail the send")
	}
	if !errors.Is(err, channel.ErrRefused) {
		t.Errorf("err = %v, want ErrRefused", err)
	}
	if f.received() != "" {
		t.Error("the message was delivered to the recipients that were accepted")
	}
}

func TestUnreachableRelay(t *testing.T) {
	ch := &config.Channel{
		Name: "notices", Type: config.TypeEmail, Host: "127.0.0.1", Port: 1,
		Encryption: config.EncNone, SendTimeout: config.Duration(config.DefaultSendTimeout),
	}
	_, err := New(ch).Send(context.Background(), msg())
	if !errors.Is(err, channel.ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}
