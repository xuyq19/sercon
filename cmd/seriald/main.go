// Command seriald runs on the jump host, the machine the serial adapters are
// physically plugged into.
//
// It has three faces. "capture" is the daemon that owns the devices.
// "session" is the thin pipe an SSH connection drops into. "list", "status" and
// "stop" are operator commands. Which one runs is decided entirely by argv, so
// there is a single binary to copy onto the jump host.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"seriald/internal/audit"
	"seriald/internal/config"
	"seriald/internal/daemon"
	"seriald/internal/hub"
	"seriald/internal/ipc"
	"seriald/internal/proto"
	"seriald/internal/serialport"
	"seriald/internal/version"
	"seriald/internal/winconsole"
)

// hint is shown when someone double-clicks the binary, where the console would
// otherwise close before the usage text could be read.
const hint = `On the jump host, start the daemon with:

    seriald capture

From your own machine, use the sctl client:

    sctl ls -t user@jump
    sctl attach -t user@jump PORT`

func main() {
	code := run()
	winconsole.KeepOpen("seriald", hint)
	os.Exit(code)
}

// run holds the real entry point so that every exit path goes back through
// main and gets the double-click guard.
func run() (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "seriald: internal error: %v\n", r)
			code = 1
		}
	}()

	if len(os.Args) < 2 {
		usage()
		return 2
	}

	var err error
	switch os.Args[1] {
	case "session":
		err = cmdSession(os.Args[2:])
	case "capture":
		err = cmdCapture(os.Args[2:])
	case "list", "ls":
		err = cmdList(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "stop":
		err = cmdStop(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(version.Line("seriald", proto.Version))
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "seriald: unknown command %q\n\n", os.Args[1])
		usage()
		return 2
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "seriald: "+err.Error())
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `seriald - serial console daemon for a jump host

usage:
  seriald capture [--socket PATH]      run the daemon (usually started for you)
  seriald session [--socket PATH]      ensure the daemon exists, then pipe stdio to it
  seriald session --direct             serve this connection without any daemon
  seriald list [--json]                list ports and who holds them
  seriald status                       report whether the daemon is running
  seriald stop                          ask the daemon to shut down
  seriald version

config: `+configPath()+`
`)
}

func configPath() string {
	p, err := config.Path()
	if err != nil {
		return "(unavailable)"
	}
	return p
}

// cmdSession is a dumb pipe. It does not resolve ports or speak the protocol:
// the client talks to the daemon end to end, so none of the daemon's
// complexity leaks into the SSH layer.
func cmdSession(args []string) error {
	fs := flag.NewFlagSet("session", flag.ExitOnError)
	direct := fs.Bool("direct", false, "serve this connection in-process, without a daemon")
	socket := fs.String("socket", "", "daemon socket path (default: per-user runtime dir)")
	exe := fs.String("exe", "", "seriald binary used to start the daemon")
	_ = fs.Parse(args)

	if *direct {
		return runDirect()
	}

	sock, err := resolveSocket(*socket)
	if err != nil {
		return err
	}

	exePath := *exe
	if exePath == "" {
		exePath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate own binary: %w", err)
		}
	}

	if err := daemon.Ensure(sock, exePath, []string{"capture", "--socket", sock},
		daemon.LogPath(filepath.Dir(sock))); err != nil {
		return err
	}

	conn, err := ipc.Dial(sock, 3*time.Second)
	if err != nil {
		return fmt.Errorf("connect to daemon: %w", err)
	}
	defer conn.Close()

	return pipe(conn)
}

func runDirect() error {
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	m, err := hub.New(hub.OptionsFrom(cfg))
	if err != nil {
		return err
	}
	if err := m.Start(); err != nil {
		return err
	}
	defer m.Close()

	return m.ServeConn(stdio{}, "direct")
}

// pipe copies bytes both ways until either side finishes.
func pipe(conn net.Conn) error {
	done := make(chan error, 2)

	go func() {
		_, err := io.Copy(conn, os.Stdin)
		// Signal end of input without dropping the read half, so output still
		// in flight from the target machine reaches the terminal.
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
		done <- err
	}()
	go func() {
		_, err := io.Copy(os.Stdout, conn)
		done <- err
	}()

	// Whichever direction ends first, tear the other one down. Otherwise the
	// process outlives the session, holding the socket open.
	err := <-done
	conn.Close()
	<-done
	return err
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return nil }

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	socket := fs.String("socket", "", "daemon socket path")
	_ = fs.Parse(args)

	sock, err := resolveSocket(*socket)
	if err != nil {
		return err
	}

	cfg, err := config.Load("")
	if err != nil {
		return err
	}

	// Bind before touching any hardware. Losing the race to an existing daemon
	// should cost nothing, and this is the single-instance gate on both
	// platforms: the kernel refuses a second bind on the same path.
	ln, err := ipc.Listen(filepath.Dir(sock), sock)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(sock) }()

	m, err := hub.New(hub.OptionsFrom(cfg))
	if err != nil {
		ln.Close()
		return err
	}
	if err := m.Start(); err != nil {
		ln.Close()
		return err
	}

	m.Audit().Log(audit.Event{
		Event:  audit.EventDaemonStart,
		Detail: fmt.Sprintf("pid=%d socket=%s", os.Getpid(), sock),
	})
	fmt.Fprintf(os.Stderr, "seriald: capture daemon pid=%d socket=%s\n", os.Getpid(), sock)

	stop := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		select {
		case s := <-sig:
			fmt.Fprintf(os.Stderr, "seriald: signal %v, shutting down\n", s)
		case <-m.ShutdownRequested():
			fmt.Fprintln(os.Stderr, "seriald: shutdown requested by client")
		case <-stop:
			return
		}
		close(stop)
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if isClosed(stop) {
				break
			}
			// A transient accept failure (descriptor exhaustion, say) must not
			// kill every attached session, but it must not spin either.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func(c net.Conn) {
			// The endpoint is a unix socket, which has no meaningful peer
			// address — an unnamed socket reports "@". The identity that
			// matters is the user, and that comes from the handshake.
			_ = m.ServeConn(c, "local")
		}(conn)
	}

	ln.Close()
	return m.Close()
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	socket := fs.String("socket", "", "daemon socket path")
	_ = fs.Parse(args)

	sock, err := resolveSocket(*socket)
	if err != nil {
		return err
	}

	// Prefer the daemon: only it knows who holds which port. Fall back to a
	// direct scan so listing works before anything has ever attached.
	if ports, ok := queryPorts(sock); ok {
		return printPorts(ports, *asJSON)
	}

	devs, derr := serialport.Discover(nil)
	if derr != nil {
		// Say why the list is empty, but an empty list is still a valid answer.
		fmt.Fprintf(os.Stderr, "seriald: device scan failed: %v\n", derr)
		return printPorts(nil, *asJSON)
	}
	baud := 115200
	if cfg, cerr := config.Load(""); cerr == nil && cfg.Baud > 0 {
		baud = cfg.Baud
	}
	ports := make([]proto.PortInfo, 0, len(devs))
	for _, d := range devs {
		ports = append(ports, proto.PortInfo{
			Ref:    d.Ref,
			Dev:    d.Dev,
			Desc:   d.Desc,
			Baud:   baud,
			Online: serialport.Exists(d.Dev),
		})
	}
	return printPorts(ports, *asJSON)
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	socket := fs.String("socket", "", "daemon socket path")
	_ = fs.Parse(args)

	sock, err := resolveSocket(*socket)
	if err != nil {
		return err
	}

	if !ipc.Alive(sock) {
		fmt.Printf("daemon   not running\nsocket   %s\n", sock)
		return nil
	}

	fmt.Printf("daemon   running\nsocket   %s\n", sock)
	if ports, ok := queryPorts(sock); ok {
		online, held := 0, 0
		for _, p := range ports {
			if p.Online {
				online++
			}
			if p.Owner != "" {
				held++
			}
		}
		fmt.Printf("ports    %d (%d online, %d held)\n", len(ports), online, held)
	}
	return nil
}

func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	socket := fs.String("socket", "", "daemon socket path")
	_ = fs.Parse(args)

	sock, err := resolveSocket(*socket)
	if err != nil {
		return err
	}

	conn, err := ipc.Dial(sock, 2*time.Second)
	if err != nil {
		fmt.Println("daemon   not running")
		return nil
	}
	defer conn.Close()

	wr := proto.NewWriter(conn)
	rd := proto.NewReader(conn)
	if err := wr.Ctrl(&proto.Message{Op: proto.OpShutdown}); err != nil {
		return fmt.Errorf("send shutdown: %w", err)
	}

	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if f, err := rd.Next(); err == nil && f.Type == proto.TypeCtrl {
		var msg proto.Message
		if json.Unmarshal(f.Payload, &msg) == nil && msg.Text != "" {
			fmt.Println("daemon   " + msg.Text)
		}
	}

	for i := 0; i < 50; i++ {
		if !ipc.Alive(sock) {
			fmt.Println("daemon   stopped")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not stop within 5s")
}

// queryPorts asks the running daemon for its port table.
func queryPorts(sock string) ([]proto.PortInfo, bool) {
	conn, err := ipc.Dial(sock, 700*time.Millisecond)
	if err != nil {
		return nil, false
	}
	defer conn.Close()

	wr := proto.NewWriter(conn)
	rd := proto.NewReader(conn)
	if err := wr.Ctrl(&proto.Message{Op: proto.OpList}); err != nil {
		return nil, false
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	for {
		f, err := rd.Next()
		if err != nil {
			return nil, false
		}
		if f.Type != proto.TypeCtrl {
			continue
		}
		var msg proto.Message
		if err := json.Unmarshal(f.Payload, &msg); err != nil {
			continue
		}
		if msg.Op == proto.OpPorts {
			return msg.Ports, true
		}
	}
}

func printPorts(ports []proto.PortInfo, asJSON bool) error {
	if asJSON {
		// Encode an empty list as [], not null: the client decodes into a
		// slice and should not have to special-case the empty case.
		if ports == nil {
			ports = []proto.PortInfo{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(ports)
	}

	if len(ports) == 0 {
		fmt.Println("no serial ports found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REF\tDEV\tBAUD\tSTATE\tOWNER\tOBS\tLOG")
	for _, p := range ports {
		state := "offline"
		if p.Online {
			state = "online"
		}
		owner := or(p.Owner, "-")
		observers := "-"
		if p.Observers > 0 {
			observers = strconv.Itoa(p.Observers)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			p.Ref, p.Dev, p.Baud, state, owner, observers, or(p.Log, "-"))
	}
	return w.Flush()
}

func resolveSocket(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	_, sock, err := ipc.Endpoint()
	return sock, err
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
