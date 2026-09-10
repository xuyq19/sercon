package relay

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	// helloTimeout bounds how long a connection may sit silent before sending
	// its registration line. Anything longer is a port scanner.
	helloTimeout = 20 * time.Second

	// maxHelloLen caps the registration line.
	maxHelloLen = 4096

	// minTokenLen is enforced so that a room cannot be protected by a token
	// short enough to guess.
	minTokenLen = 16
)

// Server pairs publishers with attachers and copies bytes between them.
//
// It is deliberately ignorant of everything above the byte stream: it does not
// parse the protocol, and it cannot read the TLS that runs through it. That is
// what makes it safe to run somewhere the operator does not fully trust.
type Server struct {
	// PairTimeout bounds how long an attacher waits for an idle publisher.
	// Zero means a sensible default.
	PairTimeout time.Duration

	// Logf, if set, receives one line per lifecycle event.
	Logf func(format string, args ...any)

	mu    sync.Mutex
	rooms map[string]*room
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) timeout() time.Duration {
	if s.PairTimeout <= 0 {
		return 30 * time.Second
	}
	return s.PairTimeout
}

// ListenAndServe accepts connections until the listener is closed.
func (s *Server) ListenAndServe(ln net.Listener) error {
	if s.rooms == nil {
		s.rooms = map[string]*room{}
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.ServeConn(conn)
	}
}

type room struct {
	name  string
	token string
	// idle holds publishers waiting to be paired, and waiting holds attachers
	// in the same position. A publisher occupies one connection per slot it
	// asked for, so concurrency is bounded by slots rather than by a
	// multiplexing layer.
	idle    []*peer
	waiting []*peer
}

type peer struct {
	conn   net.Conn
	role   Role
	room   string
	remote string

	// partner is written under the server lock before paired is closed, and
	// read only after it, so the channel close is what publishes it.
	partner  *peer
	paired   chan struct{}
	closeOne sync.Once
}

func (p *peer) done() <-chan struct{} { return p.paired }

// ServeConn runs one relay connection: register, wait to be paired, then copy
// bytes in both directions until either side finishes.
func (s *Server) ServeConn(conn net.Conn) {
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))
	line, err := readLine(conn, maxHelloLen)
	if err != nil {
		s.logf("relay: handshake from %s: %v", conn.RemoteAddr(), err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	var h hello
	if err := json.Unmarshal(line, &h); err != nil {
		writeReply(conn, reply{Error: "malformed registration"})
		return
	}
	switch {
	case h.Room == "":
		writeReply(conn, reply{Error: "no room given"})
		return
	case h.Role != RolePublish && h.Role != RoleAttach:
		writeReply(conn, reply{Error: "role must be publish or attach"})
		return
	case len(h.Token) < minTokenLen:
		writeReply(conn, reply{Error: fmt.Sprintf("token must be at least %d characters", minTokenLen)})
		return
	}

	p := &peer{
		conn:   conn,
		role:   h.Role,
		room:   h.Room,
		remote: conn.RemoteAddr().String(),
		paired: make(chan struct{}),
	}

	partner, err := s.register(p, h.Token)
	if err != nil {
		writeReply(conn, reply{Error: err.Error()})
		s.logf("relay: reject %s room=%q role=%s: %v", p.remote, h.Room, h.Role, err)
		return
	}

	if err := writeReply(conn, reply{OK: true, State: StateWaiting}); err != nil {
		s.unregister(p)
		return
	}
	s.logf("relay: %s room=%q role=%s waiting", p.remote, p.room, p.role)

	if partner == nil {
		// Nobody to talk to yet. Wait, but not forever: an attacher left
		// hanging gives the operator no way to tell a slow console from an
		// offline one.
		select {
		case <-p.paired:
			partner = p.partner
		case <-time.After(s.timeout()):
			s.unregister(p)
			writeReply(conn, reply{Error: "no console is publishing this room"})
			s.logf("relay: %s room=%q role=%s timed out", p.remote, p.room, p.role)
			return
		}
	}

	if err := writeReply(conn, reply{OK: true, State: StatePaired}); err != nil {
		s.closeBoth(p, partner)
		return
	}
	s.logf("relay: room=%q paired %s(%s) <-> %s(%s)",
		p.room, p.role, p.remote, partner.role, partner.remote)

	s.splice(p, partner)
}

var errTokenMismatch = errors.New("token does not match this room")

// register adds the peer to its room and returns a partner if one is available.
func (s *Server) register(p *peer, token string) (*peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rooms == nil {
		s.rooms = map[string]*room{}
	}

	r, ok := s.rooms[p.room]
	if !ok {
		r = &room{name: p.room, token: token}
		s.rooms[p.room] = r
	} else if subtle.ConstantTimeCompare([]byte(r.token), []byte(token)) != 1 {
		return nil, errTokenMismatch
	}

	switch p.role {
	case RolePublish:
		if len(r.waiting) > 0 {
			partner := r.waiting[0]
			r.waiting = r.waiting[1:]
			p.partner, partner.partner = partner, p
			close(partner.paired)
			return partner, nil
		}
		r.idle = append(r.idle, p)
		return nil, nil

	default: // RoleAttach
		if len(r.idle) > 0 {
			partner := r.idle[0]
			r.idle = r.idle[1:]
			p.partner, partner.partner = partner, p
			close(partner.paired)
			return partner, nil
		}
		r.waiting = append(r.waiting, p)
		return nil, nil
	}
}

func (s *Server) unregister(p *peer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.rooms[p.room]
	if !ok {
		return
	}
	r.idle = remove(r.idle, p)
	r.waiting = remove(r.waiting, p)

	if len(r.idle) == 0 && len(r.waiting) == 0 {
		delete(s.rooms, p.room)
	}
}

func remove(list []*peer, p *peer) []*peer {
	out := list[:0]
	for _, q := range list {
		if q != p {
			out = append(out, q)
		}
	}
	return out
}

// Rooms reports the active room names, for status output.
func (s *Server) Rooms() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.rooms))
	for name := range s.rooms {
		out = append(out, name)
	}
	return out
}

// splice copies bytes until either direction ends, then tears both down.
//
// Each direction runs in its own goroutine because a console session is
// half-duplex in practice: the target may be silent for minutes while the
// operator types, and waiting for both to finish would deadlock.
func (s *Server) splice(a, b *peer) {
	done := make(chan struct{}, 2)

	go func() {
		_, _ = io.Copy(a.conn, b.conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b.conn, a.conn)
		done <- struct{}{}
	}()

	<-done
	s.closeBoth(a, b)
	<-done
}

func (s *Server) closeBoth(a, b *peer) {
	a.closeOne.Do(func() { a.conn.Close() })
	b.closeOne.Do(func() { b.conn.Close() })
	s.unregister(a)
	s.unregister(b)
}

func writeReply(conn net.Conn, r reply) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(raw, '\n'))
	return err
}

// readLine reads up to max bytes, without buffering.
//
// A bufio.Reader would be the obvious choice and the wrong one: after the
// handshake the very next bytes on this connection belong to the session, and
// anything sitting in a buffer would be silently dropped on the floor.
func readLine(r io.Reader, max int) ([]byte, error) {
	buf := make([]byte, 0, 128)
	one := make([]byte, 1)

	for len(buf) < max {
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return trimCR(buf), nil
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			if len(buf) > 0 {
				return trimCR(buf), nil
			}
			return nil, err
		}
	}
	return nil, fmt.Errorf("registration line exceeds %d bytes", max)
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}
