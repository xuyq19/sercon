package relay

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Publisher exposes a local sercond socket through a relay.
//
// Both ends of a relay session connect outbound. That is the whole trick: a
// machine behind NAT needs no port forwarding, no inbound firewall rule, and no
// stable address, because it never accepts a connection — it makes one and
// keeps it.
type Publisher struct {
	// Relay is the relay's host:port.
	Relay string
	// Room names this console on the relay.
	Room string
	// Token authenticates to the relay and must match the attacher's.
	Token string
	// Sock is the local daemon socket to expose.
	Sock string
	// Slots is how many concurrent sessions to serve. Each slot holds one
	// relay connection, so this is also the concurrency limit.
	Slots int
	// Identity is the TLS certificate the attacher pins.
	Identity *Identity
	// Logf receives one line per lifecycle event.
	Logf func(format string, args ...any)

	done chan struct{}
	once sync.Once
}

const (
	dialTimeout = 15 * time.Second
	// tlsTimeout bounds the end-to-end handshake once the relay has paired the
	// two ends. Without it a peer that pairs and then says nothing would hold
	// a slot indefinitely.
	tlsTimeout = 30 * time.Second

	maxBackoff = 15 * time.Second
)

func (p *Publisher) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// Run starts every slot and blocks until Stop is called.
func (p *Publisher) Run() error {
	if p.Slots <= 0 {
		p.Slots = 4
	}
	p.done = make(chan struct{})

	p.logf("publish: room %q -> relay %s, %d slot(s), socket %s",
		p.Room, p.Relay, p.Slots, p.Sock)

	var wg sync.WaitGroup
	for i := 0; i < p.Slots; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			p.slot(n)
		}(i)
	}
	wg.Wait()
	return nil
}

// Stop asks every slot to finish.
func (p *Publisher) Stop() {
	p.once.Do(func() {
		if p.done != nil {
			close(p.done)
		}
	})
}

func (p *Publisher) slot(n int) {
	backoff := time.Second

	for {
		if p.stopping() {
			return
		}

		paired, err := p.serve()
		if err != nil && !p.stopping() {
			p.logf("publish: slot %d: %v", n, err)
		}
		if paired {
			// The slot did real work, so the next failure is a fresh problem
			// rather than a continuation of an old one.
			backoff = time.Second
			continue
		}

		select {
		case <-p.done:
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (p *Publisher) stopping() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// serve runs one slot's lifetime: register, wait to be paired, then bridge the
// end-to-end TLS session to the local daemon socket.
func (p *Publisher) serve() (paired bool, err error) {
	conn, err := net.DialTimeout("tcp", p.Relay, dialTimeout)
	if err != nil {
		return false, fmt.Errorf("dial relay %s: %w", p.Relay, err)
	}
	defer conn.Close()

	// Closing the connection is what unblocks a slot parked on a read when the
	// publisher is asked to stop.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-p.done:
			conn.Close()
		case <-stop:
		}
	}()

	raw, err := json.Marshal(hello{Room: p.Room, Token: p.Token, Role: RolePublish})
	if err != nil {
		return false, err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return false, err
	}

	if err := expectState(conn, StateWaiting); err != nil {
		return false, err
	}
	// Blocks until an attacher turns up. That is the point of an idle slot.
	if err := expectState(conn, StatePaired); err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), tlsTimeout)
	defer cancel()

	tlsConn := tls.Server(conn, p.Identity.TLSConfig())
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return false, fmt.Errorf("end-to-end TLS with the client: %w", err)
	}

	local, err := net.DialTimeout("unix", p.Sock, 5*time.Second)
	if err != nil {
		return true, fmt.Errorf("local daemon socket %s: %w", p.Sock, err)
	}
	defer local.Close()

	spliceConns(tlsConn, local)
	return true, nil
}

// Attach connects to a console through a relay and returns a connection
// carrying the sercond protocol.
//
// The returned connection is end-to-end encrypted: the relay forwards bytes it
// cannot read, and the certificate is pinned to the fingerprint in the ticket.
func Attach(t Ticket, timeout time.Duration) (net.Conn, error) {
	if !t.Complete() {
		return nil, errors.New("relay: incomplete ticket (relay, room, token and fingerprint are all required)")
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}

	conn, err := net.DialTimeout("tcp", t.Relay, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial relay %s: %w", t.Relay, err)
	}

	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}

	raw, err := json.Marshal(hello{Room: t.Room, Token: t.Token, Role: RoleAttach})
	if err != nil {
		return fail(err)
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return fail(err)
	}

	if err := expectState(conn, StateWaiting); err != nil {
		return fail(err)
	}

	// The wait for a publisher is bounded by the caller's timeout rather than
	// the relay's, so the message a user sees names the thing they waited for.
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	if err := expectState(conn, StatePaired); err != nil {
		_ = conn.SetReadDeadline(time.Time{})
		return fail(fmt.Errorf("waiting for the console to connect: %w", err))
	}
	_ = conn.SetReadDeadline(time.Time{})

	ctx, cancel := context.WithTimeout(context.Background(), tlsTimeout)
	defer cancel()

	tlsConn := tls.Client(conn, t.tlsConfig())
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fail(fmt.Errorf("end-to-end TLS with the console: %w", err))
	}
	return tlsConn, nil
}

// expectState reads one reply and insists it carries the wanted state.
func expectState(conn net.Conn, want string) error {
	line, err := readLine(conn, maxHelloLen)
	if err != nil {
		return err
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return fmt.Errorf("relay: malformed reply %q", line)
	}
	if !r.OK {
		if r.Error == "" {
			r.Error = "relay refused the connection"
		}
		return errors.New(r.Error)
	}
	if want != "" && r.State != want {
		return fmt.Errorf("relay: expected state %q, got %q", want, r.State)
	}
	return nil
}

// spliceConns copies both directions until either finishes.
func spliceConns(a, b net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()

	<-done
	a.Close()
	b.Close()
	<-done
}
