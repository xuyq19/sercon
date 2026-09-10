// Package hub owns the serial ports on a jump host.
//
// One Port exists per discovered device and lives for the lifetime of the
// daemon, even while the device is unplugged. That durability is deliberate: it
// keeps the port's identity, its log file and its backlog stable across the
// replug that would otherwise look like a brand new machine.
package hub

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"sercon/internal/audit"
	"sercon/internal/config"
	"sercon/internal/portlog"
	"sercon/internal/proto"
	"sercon/internal/serialport"
)

// Options configures a Manager.
type Options struct {
	LogDir       string
	AuditDir     string
	DefaultBaud  int
	StampLogs    bool
	AllowObserve bool
	MaxObservers int
	AutoOpen     bool
	ScanGlobs    []string
	BacklogBytes int
	Aliases      map[string]config.PortDef
}

// OptionsFrom converts a loaded configuration into manager options.
func OptionsFrom(cfg config.Config) Options {
	return Options{
		LogDir:       cfg.LogDir,
		AuditDir:     cfg.AuditDir,
		DefaultBaud:  cfg.Baud,
		StampLogs:    cfg.LogsStamped(),
		AllowObserve: cfg.ObserversAllowed(),
		MaxObservers: cfg.MaxObservers,
		AutoOpen:     cfg.OpensAutomatically(),
		ScanGlobs:    cfg.ScanGlobs,
		Aliases:      cfg.Aliases(),
	}
}

// rediscoverInterval is how often new devices are looked for. A repluged
// adapter shows up within one interval.
const rediscoverInterval = 5 * time.Second

// Manager tracks every port on this host and every attached client.
type Manager struct {
	opts  Options
	audit *audit.Logger

	mu      sync.Mutex
	ports   map[string]*Port
	conns   map[*Conn]struct{}
	closed  bool
	scanErr error

	done chan struct{}
	wg   sync.WaitGroup

	stopReq  chan struct{}
	stopOnce sync.Once
}

// New prepares a manager. Audit logging starts immediately; ports are opened
// by Start.
func New(opts Options) (*Manager, error) {
	if opts.LogDir == "" {
		return nil, errors.New("hub: LogDir is required")
	}
	if opts.AuditDir == "" {
		opts.AuditDir = filepath.Join(filepath.Dir(opts.LogDir), "audit")
	}
	if opts.DefaultBaud == 0 {
		opts.DefaultBaud = 115200
	}
	if opts.BacklogBytes <= 0 {
		opts.BacklogBytes = 64 << 10
	}

	al, err := audit.New(opts.AuditDir)
	if err != nil {
		return nil, err
	}
	return &Manager{
		opts:    opts,
		audit:   al,
		ports:   make(map[string]*Port),
		conns:   make(map[*Conn]struct{}),
		done:    make(chan struct{}),
		stopReq: make(chan struct{}),
	}, nil
}

// Start discovers ports and begins watching for new ones.
//
// A first-scan failure is recorded rather than returned: a daemon that is
// already holding working ports should not die because one enumeration call
// failed, and the periodic rescan will recover on its own.
func (m *Manager) Start() error {
	if err := m.Sync(); err != nil {
		m.mu.Lock()
		m.scanErr = err
		m.mu.Unlock()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(rediscoverInterval)
		defer t.Stop()
		for {
			select {
			case <-m.done:
				return
			case <-t.C:
				_ = m.Sync()
			}
		}
	}()
	return nil
}

// Close stops every port worker and releases the logs.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.done)
	ports := make([]*Port, 0, len(m.ports))
	for _, p := range m.ports {
		ports = append(ports, p)
	}
	conns := make([]*Conn, 0, len(m.conns))
	for c := range m.conns {
		conns = append(conns, c)
	}
	m.mu.Unlock()

	// Dropping the clients first means nobody is mid-write when the devices go.
	for _, c := range conns {
		c.kill()
	}
	for _, p := range ports {
		p.shutdown()
	}
	m.wg.Wait()
	for _, p := range ports {
		p.release()
	}

	m.audit.Log(audit.Event{Event: audit.EventDaemonStop})
	return m.audit.Close()
}

// Sync reconciles the manager's port set with what is currently plugged in.
// Ports that vanish stay registered and go offline; nothing is ever dropped, so
// a returning device resumes its own log file.
func (m *Manager) Sync() error {
	devs, err := serialport.Discover(m.opts.ScanGlobs)

	m.mu.Lock()
	m.scanErr = err
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if m.closed {
		m.mu.Unlock()
		return errors.New("hub: manager is closed")
	}

	var fresh []*Port
	for _, d := range devs {
		if p, ok := m.ports[d.Ref]; ok {
			p.mu.Lock()
			p.link = d.Link
			p.dev = d.Dev
			p.mu.Unlock()
			continue
		}
		p := m.newPort(d)
		m.ports[d.Ref] = p
		fresh = append(fresh, p)
	}
	autoOpen := m.opts.AutoOpen
	m.mu.Unlock()

	// Starting workers outside the manager lock keeps the lock order
	// manager -> port consistent with every other path.
	if autoOpen {
		for _, p := range fresh {
			p.ensureWorker()
		}
	}
	return nil
}

func (m *Manager) newPort(d serialport.Device) *Port {
	baud := m.opts.DefaultBaud
	desc := d.Desc
	if a, ok := m.opts.Aliases[d.Ref]; ok {
		if a.Baud > 0 {
			baud = a.Baud
		}
		if a.Desc != "" {
			desc = a.Desc
		}
	}

	p := &Port{
		mgr:   m,
		ref:   d.Ref,
		link:  d.Link,
		dev:   d.Dev,
		desc:  desc,
		baud:  baud,
		ring:  newRing(m.opts.BacklogBytes),
		conns: make(map[*Conn]struct{}),
	}

	// A port that cannot write its log is broken in the one way that matters
	// most: the daemon exists so that output is captured whether or not anyone
	// is watching. Swallowing this would leave the window looking healthy while
	// nothing was being recorded, so the failure is surfaced on the port itself
	// where the listing and the GUI both show it.
	if lw, err := portlog.New(m.opts.LogDir, d.Ref, m.opts.StampLogs); err != nil {
		p.logErr = "log unavailable: " + err.Error()
	} else {
		lw.Describe(d.Dev, baud)
		p.logw = lw
	}
	return p
}

// Ports returns a snapshot for listings.
func (m *Manager) Ports() []proto.PortInfo {
	m.mu.Lock()
	list := make([]*Port, 0, len(m.ports))
	for _, p := range m.ports {
		list = append(list, p)
	}
	m.mu.Unlock()

	out := make([]proto.PortInfo, 0, len(list))
	for _, p := range list {
		out = append(out, p.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// Resolve maps an operator-typed reference to a port.
//
// Exact matches win. Otherwise a unique substring is accepted, which is what
// makes "sercon attach FT232" work without pasting a 40-character by-id name.
func (m *Manager) Resolve(ref string) (*Port, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ref == "" {
		if len(m.ports) == 1 {
			for _, p := range m.ports {
				return p, nil
			}
		}
		return nil, fmt.Errorf("no port given and %d ports are available (run 'list')", len(m.ports))
	}

	if p, ok := m.ports[ref]; ok {
		return p, nil
	}
	for _, p := range m.ports {
		if p.dev == ref || filepath.Base(p.dev) == ref || strings.EqualFold(p.ref, ref) {
			return p, nil
		}
	}

	needle := strings.ToLower(ref)
	var hits []*Port
	for _, p := range m.ports {
		if strings.Contains(strings.ToLower(p.ref), needle) ||
			strings.Contains(strings.ToLower(p.desc), needle) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no serial port matches %q", ref)
	case 1:
		return hits[0], nil
	default:
		names := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, h.ref)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%q is ambiguous, matches %s", ref, strings.Join(names, ", "))
	}
}

// Audit exposes the audit logger to command code.
func (m *Manager) Audit() *audit.Logger { return m.audit }

// ScanErr reports the last device-enumeration failure, if any. It is surfaced
// in status output so a silently empty port list has a visible reason.
func (m *Manager) ScanErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scanErr
}

// ShutdownRequested is closed when a client asks the daemon to stop. The
// daemon's own signal loop watches it alongside SIGINT and SIGTERM.
func (m *Manager) ShutdownRequested() <-chan struct{} { return m.stopReq }

// RequestShutdown asks the daemon to exit cleanly.
func (m *Manager) RequestShutdown() {
	m.stopOnce.Do(func() { close(m.stopReq) })
}

func (m *Manager) stopped() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// Port is one serial device plus everyone attached to it.
type Port struct {
	mgr  *Manager
	ref  string
	link string
	dev  string
	desc string
	baud int
	ring *ring

	mu      sync.Mutex
	sp      *serialport.Port
	online  bool
	running bool
	lastErr string
	// logErr is kept apart from lastErr on purpose. lastErr describes the
	// device and is cleared every time the port comes back online; the log
	// failure is a property of the daemon's setup and has to survive that.
	logErr  string
	logw    *portlog.Writer
	conns   map[*Conn]struct{}
	owner   *Conn
	read    int64
	written int64
}

// Info produces the listing entry for this port.
func (p *Port) Info() proto.PortInfo {
	p.mu.Lock()
	observers := 0
	for c := range p.conns {
		if c != p.owner {
			observers++
		}
	}
	owner := ""
	if p.owner != nil {
		owner = p.owner.label
	}
	lastErr := p.lastErr
	if p.logErr != "" {
		if lastErr != "" {
			lastErr = p.logErr + "; " + lastErr
		} else {
			lastErr = p.logErr
		}
	}

	info := proto.PortInfo{
		Ref:       p.ref,
		Dev:       p.dev,
		Desc:      p.desc,
		Baud:      p.baud,
		Online:    p.online,
		Owner:     owner,
		Observers: observers,
		LastErr:   lastErr,
	}
	logw := p.logw
	p.mu.Unlock()

	if logw != nil {
		info.Log = logw.Path()
	}
	return info
}

// ensureWorker starts the read loop the first time it is needed.
//
// It takes the manager lock so that registering with the WaitGroup cannot race
// with Close's Wait — an Add after Wait has begun is a panic, not a leak.
func (p *Port) ensureWorker() {
	m := p.mgr

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		m.mu.Unlock()
		return
	}
	p.running = true
	p.mu.Unlock()
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		p.worker()
	}()
}

// worker holds the device open and republishes everything it reads.
//
// It never gives up: when the device disappears it backs off and retries, so a
// target that is power-cycled or an adapter that is repluged comes back on its
// own. This is the server half of automatic reconnection.
func (p *Port) worker() {
	buf := make([]byte, 8192)
	backoff := time.Second

	for {
		if p.mgr.stopped() {
			p.release()
			return
		}

		sp, dev, err := p.openDevice()
		if err != nil {
			p.goOffline(err)
			if !sleepOrStop(p.mgr.done, backoff) {
				p.release()
				return
			}
			if backoff < 15*time.Second {
				backoff *= 2
				if backoff > 15*time.Second {
					backoff = 15 * time.Second
				}
			}
			continue
		}

		backoff = time.Second
		p.goOnline(sp, dev)

		for {
			n, rerr := sp.Read(buf, 250*time.Millisecond)
			if n > 0 {
				p.fanout(buf[:n])
			}
			if rerr != nil {
				// A read failure during shutdown is just the device being
				// closed underneath us, not the target going away. Marking the
				// port offline there would put a misleading line in the log.
				if !p.mgr.stopped() {
					p.goOffline(rerr)
				}
				break
			}
			if p.mgr.stopped() {
				break
			}
		}

		sp.Close()
		p.clearDevice()
		if p.mgr.stopped() {
			p.release()
			return
		}
	}
}

// openDevice resolves the reference afresh on every attempt. On Linux the by-id
// symlink is what returns a repluged adapter to its old identity even after its
// ttyUSB number changed, and on Windows the COM number is all there is.
func (p *Port) openDevice() (*serialport.Port, string, error) {
	p.mu.Lock()
	link := p.link
	baud := p.baud
	p.mu.Unlock()

	dev := link
	if resolved, err := filepath.EvalSymlinks(link); err == nil {
		dev = resolved
	}

	sp, err := serialport.Open(dev, baud, false)
	if err != nil {
		return nil, dev, err
	}
	return sp, dev, nil
}

func (p *Port) goOnline(sp *serialport.Port, dev string) {
	p.mu.Lock()
	p.sp = sp
	p.dev = dev
	was := p.online
	p.online = true
	p.lastErr = ""
	baud := p.baud
	logw := p.logw
	conns := connsOf(p.conns)
	p.mu.Unlock()

	if was {
		return
	}

	p.mgr.audit.Log(audit.Event{
		Event: audit.EventPortOnline, Port: p.ref, Dev: dev,
		Detail: fmt.Sprintf("baud=%d", baud),
	})
	if logw != nil {
		_ = logw.Note("online: %s @ %d baud", dev, baud)
	}
	notice(conns, fmt.Sprintf("--- port online: %s @ %d ---", dev, baud))
}

func (p *Port) goOffline(err error) {
	p.mu.Lock()
	was := p.online
	p.online = false
	p.lastErr = err.Error()
	dev := p.dev
	logw := p.logw
	conns := connsOf(p.conns)
	p.mu.Unlock()

	if !was {
		return
	}

	p.mgr.audit.Log(audit.Event{
		Event: audit.EventPortOffline, Port: p.ref, Dev: dev, Detail: err.Error(),
	})
	if logw != nil {
		_ = logw.Note("offline: %v", err)
	}
	notice(conns, fmt.Sprintf("--- port offline: %v ---", err))
}

func (p *Port) clearDevice() {
	p.mu.Lock()
	p.sp = nil
	p.mu.Unlock()
}

// fanout publishes console output to the log, the backlog and every client.
//
// The buffer is copied first: the caller reuses it, and clients are served
// asynchronously, so a shared slice would be overwritten under them.
func (p *Port) fanout(b []byte) {
	cp := make([]byte, len(b))
	copy(cp, b)

	p.ring.Write(cp)

	p.mu.Lock()
	logw := p.logw
	p.read += int64(len(cp))
	for c := range p.conns {
		c.enqueue(proto.TypeData, cp)
	}
	p.mu.Unlock()

	if logw != nil {
		_, _ = logw.Write(cp)
	}
}

// write pushes client keystrokes to the device without holding the port lock,
// so a stalled write cannot freeze the read loop or the listings.
func (p *Port) write(b []byte) (int, error) {
	p.mu.Lock()
	sp := p.sp
	p.mu.Unlock()
	if sp == nil {
		return 0, errors.New("port is offline")
	}

	n, err := sp.Write(b)
	if n > 0 {
		p.mu.Lock()
		p.written += int64(n)
		p.mu.Unlock()
	}
	return n, err
}

// sendBreak pulses the break line.
func (p *Port) sendBreak() error {
	p.mu.Lock()
	sp := p.sp
	p.mu.Unlock()
	if sp == nil {
		return errors.New("port is offline")
	}
	return sp.SendBreak()
}

// noteLog writes an operator-visible marker without blocking on a closed file.
func (p *Port) noteLog(format string, args ...any) {
	p.mu.Lock()
	logw := p.logw
	p.mu.Unlock()
	if logw != nil {
		_ = logw.Note(format, args...)
	}
}

// shutdown closes the device so the worker's pending read returns.
func (p *Port) shutdown() {
	p.mu.Lock()
	sp := p.sp
	p.sp = nil
	p.online = false
	p.mu.Unlock()

	if sp != nil {
		sp.Close()
	}
}

// release closes the log file. Idempotent, so both the worker and the manager
// can call it during shutdown.
func (p *Port) release() {
	p.mu.Lock()
	logw := p.logw
	p.logw = nil
	p.mu.Unlock()

	if logw != nil {
		_ = logw.Note("capture stopped")
		_ = logw.Close()
	}
}

// ring is a fixed-size window over the most recent console output.
//
// It backs the backlog a client receives on attach: the question that matters
// during triage is usually what the machine said just before you got there.
type ring struct {
	mu   sync.Mutex
	buf  []byte
	size int
}

func newRing(size int) *ring { return &ring{size: size} }

func (r *ring) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(p) >= r.size {
		r.buf = append(r.buf[:0], p[len(p)-r.size:]...)
		return
	}
	if len(r.buf)+len(p) > r.size {
		drop := len(r.buf) + len(p) - r.size
		n := copy(r.buf, r.buf[drop:])
		r.buf = r.buf[:n]
	}
	r.buf = append(r.buf, p...)
}

func (r *ring) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}

func connsOf(set map[*Conn]struct{}) []*Conn {
	out := make([]*Conn, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	return out
}

func notice(conns []*Conn, text string) {
	for _, c := range conns {
		c.notice(text)
	}
}

func sleepOrStop(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return false
	case <-t.C:
		return true
	}
}
