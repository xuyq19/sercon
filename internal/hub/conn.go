package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"sync"
	"time"

	"sercon/internal/audit"
	"sercon/internal/proto"
)

const (
	// outQueueDepth bounds how far a client may fall behind before it is cut
	// loose. At 115200 baud a healthy link never accumulates anything.
	outQueueDepth = 512

	// handshakeTimeout bounds how long a connection may sit silent before the
	// daemon reclaims it. Any frame resets the timer.
	idleTimeout = 10 * time.Minute
)

var errBye = errors.New("hub: client signed off")

type outMsg struct {
	typ  byte
	data []byte
}

// Conn is one attached client.
//
// Frames are never written from the goroutine that produced them. Everything
// funnels through outCh into a single writer, so a slow or wedged client costs
// its own queue and nothing else — in particular it cannot stall the port
// reader that every other session depends on.
type Conn struct {
	mgr    *Manager
	rw     io.ReadWriteCloser
	remote string
	id     string

	// Set once during the handshake, never written again. Safe to read while
	// holding a port lock.
	label  string
	user   string
	client string

	outCh chan outMsg
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup

	mu       sync.Mutex
	port     *Port
	writable bool
	written  int64
	openedAt time.Time
}

// ServeConn runs one client connection to completion.
//
// rw is a unix socket when the daemon owns the endpoint, or the SSH channel's
// stdin/stdout in --direct mode. The protocol is identical either way.
func (m *Manager) ServeConn(rw io.ReadWriteCloser, remote string) error {
	c := &Conn{
		mgr:    m,
		rw:     rw,
		remote: remote,
		id:     shortID(),
		outCh:  make(chan outMsg, outQueueDepth),
		done:   make(chan struct{}),
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		rw.Close()
		return errors.New("hub: manager is shutting down")
	}
	m.conns[c] = struct{}{}
	m.mu.Unlock()

	c.wg.Add(1)
	go c.writeLoop()

	err := c.readLoop()
	c.shutdown()
	return err
}

func (c *Conn) readLoop() error {
	watchdog := time.AfterFunc(idleTimeout, c.kill)
	defer watchdog.Stop()

	rd := proto.NewReader(c.rw)
	for {
		f, err := rd.Next()
		if err != nil {
			return err
		}
		watchdog.Reset(idleTimeout)

		switch f.Type {
		case proto.TypeData:
			c.handleInput(f.Payload)
		case proto.TypeCtrl:
			var msg proto.Message
			if err := json.Unmarshal(f.Payload, &msg); err != nil {
				c.reply(&proto.Message{Op: proto.OpError, Code: "badframe", Text: "malformed control frame"})
				continue
			}
			if err := c.handle(&msg); err != nil {
				if errors.Is(err, errBye) {
					return nil
				}
				c.reply(&proto.Message{Op: proto.OpError, Code: "failed", Text: err.Error()})
			}
		}

		select {
		case <-c.done:
			return nil
		default:
		}
	}
}

func (c *Conn) writeLoop() {
	defer c.wg.Done()

	wr := proto.NewWriter(c.rw)
	for {
		select {
		case <-c.done:
			return
		case m := <-c.outCh:
			if err := wr.Write(m.typ, m.data); err != nil {
				c.kill()
				return
			}
		}
	}
}

func (c *Conn) handle(msg *proto.Message) error {
	switch msg.Op {
	case proto.OpHello:
		return c.handleHello(msg)
	case proto.OpList:
		return c.send(&proto.Message{Op: proto.OpPorts, Ports: c.mgr.Ports()})
	case proto.OpOpen:
		return c.handleOpen(msg)
	case proto.OpClose:
		c.detach("client closed")
		return c.send(&proto.Message{Op: proto.OpClosed, Text: "detached"})
	case proto.OpPing:
		return c.send(&proto.Message{Op: proto.OpPong})
	case proto.OpBreak:
		return c.handleBreak()
	case proto.OpStatus:
		return c.send(&proto.Message{Op: proto.OpStat, Text: c.status()})
	case proto.OpShutdown:
		// Only the socket owner can reach this, so it is the same trust
		// boundary as the daemon's own signal handler.
		c.mgr.audit.Log(audit.Event{
			Event: audit.EventDaemonStop, User: c.user, Client: c.client,
			Session: c.id, Detail: "requested by client",
		})
		c.mgr.RequestShutdown()
		return c.send(&proto.Message{Op: proto.OpStat, Text: "shutting down"})
	case proto.OpBye:
		return errBye
	default:
		return fmt.Errorf("unsupported op %q", msg.Op)
	}
}

func (c *Conn) handleHello(msg *proto.Message) error {
	if c.label != "" {
		return errors.New("handshake already completed")
	}
	if msg.V != proto.Version {
		return fmt.Errorf("protocol mismatch: daemon speaks v%d, client speaks v%d", proto.Version, msg.V)
	}

	// The SSH session already authenticated this user; the socket's ownership
	// is what keeps other accounts out. USERNAME/USER is only a label for the
	// audit trail, so a missing value is not worth failing over.
	c.user = firstNonEmpty(msg.User, os.Getenv("USER"), os.Getenv("USERNAME"), currentUser())
	c.client = firstNonEmpty(msg.Client, "unknown")
	c.label = c.user + "@" + c.client

	host, _ := os.Hostname()
	c.mgr.audit.Log(audit.Event{
		Event:   audit.EventSessionOpen,
		User:    c.user,
		Client:  c.client,
		Remote:  c.remote,
		Session: c.id,
	})
	return c.send(&proto.Message{Op: proto.OpWelcome, V: proto.Version, Server: host, User: c.user})
}

func (c *Conn) handleOpen(msg *proto.Message) error {
	c.detach("reopening")

	p, err := c.mgr.Resolve(msg.Port)
	if err != nil {
		return err
	}
	wantWrite := !msg.Observe

	p.mu.Lock()

	if p.sp == nil {
		last := p.lastErr
		p.mu.Unlock()
		if last == "" {
			last = "device not present"
		}
		return fmt.Errorf("port %s is offline (%s)", p.ref, last)
	}

	if wantWrite && p.owner != nil && p.owner != c {
		holder := p.owner.label
		p.mu.Unlock()
		c.mgr.audit.Log(audit.Event{
			Event: audit.EventBusyDenied, User: c.user, Client: c.client,
			Port: p.ref, Session: c.id, Detail: "held by " + holder,
		})
		return fmt.Errorf("port %s is held by %s; use --observe to attach read-only", p.ref, holder)
	}

	if !wantWrite && !c.mgr.opts.AllowObserve {
		p.mu.Unlock()
		c.mgr.audit.Log(audit.Event{
			Event: audit.EventObserveDenied, User: c.user, Client: c.client,
			Port: p.ref, Session: c.id,
		})
		return errors.New("read-only attachment is disabled by server policy")
	}

	if !wantWrite && c.mgr.opts.MaxObservers > 0 {
		n := 0
		for o := range p.conns {
			if o != p.owner {
				n++
			}
		}
		if n >= c.mgr.opts.MaxObservers {
			p.mu.Unlock()
			return fmt.Errorf("port %s already has %d observers", p.ref, n)
		}
	}

	// The first attachment sets the line rate; later ones inherit it so a
	// second operator cannot retune the port out from under the first.
	if msg.Baud > 0 && len(p.conns) == 0 {
		p.baud = msg.Baud
	}
	baud := p.baud
	dev := p.dev
	logw := p.logw

	// The backlog is queued before this connection joins the fan-out set, so
	// live output can never overtake history.
	if backlog := p.ring.Bytes(); len(backlog) > 0 {
		c.enqueue(proto.TypeData, backlog)
	}
	p.conns[c] = struct{}{}
	if wantWrite {
		p.owner = c
	}
	ownerLabel := ""
	if p.owner != nil {
		ownerLabel = p.owner.label
	}
	observers := 0
	for o := range p.conns {
		if o != p.owner {
			observers++
		}
	}
	p.mu.Unlock()

	c.mu.Lock()
	c.port = p
	c.writable = wantWrite
	c.openedAt = time.Now()
	c.mu.Unlock()

	p.ensureWorker()

	role := "observe"
	if wantWrite {
		role = "writable"
	}
	c.mgr.audit.Log(audit.Event{
		Event: audit.EventPortOpen, User: c.user, Client: c.client,
		Port: p.ref, Dev: dev, Session: c.id, Detail: role,
	})
	if logw != nil {
		marker := "acquired by"
		if !wantWrite {
			marker = "observed by"
		}
		_ = logw.Note("%s %s (%s)", marker, c.label, role)
	}

	logPath := ""
	if logw != nil {
		logPath = logw.Path()
	}
	return c.send(&proto.Message{
		Op:        proto.OpOpened,
		Port:      p.ref,
		Baud:      baud,
		Writable:  wantWrite,
		Owner:     ownerLabel,
		Observers: observers,
		Log:       logPath,
		Since:     c.openedAt.Format(time.RFC3339),
	})
}

func (c *Conn) handleBreak() error {
	c.mu.Lock()
	p := c.port
	writable := c.writable
	c.mu.Unlock()

	if p == nil {
		return errors.New("not attached to a port")
	}
	if !writable {
		return errors.New("break requires a writable session")
	}
	if err := p.sendBreak(); err != nil {
		return err
	}
	c.mgr.audit.Log(audit.Event{
		Event: audit.EventPortBreak, User: c.user, Client: c.client, Port: p.ref, Session: c.id,
	})
	p.noteLog("break sent by %s", c.label)
	return nil
}

func (c *Conn) handleInput(b []byte) {
	c.mu.Lock()
	p := c.port
	writable := c.writable
	c.mu.Unlock()

	if p == nil || !writable {
		return
	}

	n, err := p.write(b)
	if n > 0 {
		c.mu.Lock()
		c.written += int64(n)
		c.mu.Unlock()
	}
	if err != nil {
		c.reply(&proto.Message{Op: proto.OpError, Code: "write", Text: err.Error()})
	}
}

func (c *Conn) status() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.port == nil {
		return "not attached"
	}
	role := "observe"
	if c.writable {
		role = "writable"
	}
	return fmt.Sprintf("port=%s role=%s since=%s written=%d",
		c.port.ref, role, c.openedAt.Format(time.RFC3339), c.written)
}

// detach releases the port, leaving the connection alive for a reopen.
func (c *Conn) detach(reason string) {
	c.mu.Lock()
	p := c.port
	writable := c.writable
	written := c.written
	c.port = nil
	c.writable = false
	c.mu.Unlock()

	if p == nil {
		return
	}

	p.mu.Lock()
	delete(p.conns, c)
	if p.owner == c {
		p.owner = nil
	}
	logw := p.logw
	p.mu.Unlock()

	c.mgr.audit.Log(audit.Event{
		Event: audit.EventPortClose, User: c.user, Client: c.client,
		Port: p.ref, Session: c.id, Detail: reason, Bytes: written,
	})
	if logw != nil {
		role := "observer"
		if writable {
			role = "writer"
		}
		_ = logw.Note("released by %s (%s, %s, %d bytes)", c.label, role, reason, written)
	}
}

// shutdown tears the connection down and unregisters it.
func (c *Conn) shutdown() {
	c.detach("disconnected")
	c.once.Do(func() { close(c.done) })
	c.rw.Close()
	c.wg.Wait()

	// Administrative connections — list, status, stop — never complete the
	// handshake. Recording them as sessions would bury the real ones.
	if c.label != "" {
		c.mgr.audit.Log(audit.Event{
			Event: audit.EventSessionClose, User: c.user, Client: c.client,
			Remote: c.remote, Session: c.id,
		})
	}

	c.mgr.mu.Lock()
	delete(c.mgr.conns, c)
	c.mgr.mu.Unlock()
}

// kill drops the connection from any goroutine. Detaching is left to the read
// loop's shutdown so that nothing takes a port lock from inside a fan-out.
func (c *Conn) kill() {
	c.once.Do(func() {
		close(c.done)
		c.rw.Close()
	})
}

// enqueue hands a frame to the writer, dropping the client if it is too far
// behind to keep up.
func (c *Conn) enqueue(t byte, b []byte) {
	select {
	case <-c.done:
		return
	default:
	}

	select {
	case c.outCh <- outMsg{typ: t, data: b}:
	default:
		c.kill()
	}
}

// send queues a control reply. Unlike enqueue it blocks when the queue is full,
// because a dropped welcome or open reply would leave the client stuck.
func (c *Conn) send(m *proto.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	select {
	case c.outCh <- outMsg{typ: proto.TypeCtrl, data: b}:
		return nil
	case <-c.done:
		return io.ErrClosedPipe
	}
}

// reply is send for error paths, where a closed pipe is not worth reporting.
func (c *Conn) reply(m *proto.Message) {
	_ = c.send(m)
}

// notice displays a server-side event in the client's terminal.
func (c *Conn) notice(text string) {
	b, err := json.Marshal(&proto.Message{Op: proto.OpNotice, Text: text})
	if err != nil {
		return
	}
	select {
	case c.outCh <- outMsg{typ: proto.TypeCtrl, data: b}:
	default:
	}
}

func shortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}
