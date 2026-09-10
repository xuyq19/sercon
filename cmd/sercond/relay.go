package main

// Relay and publish are implemented but deliberately NOT registered in main.
//
// They were written when network traversal came up mid-project and then
// deprioritised. Leaving them unreachable costs nothing and keeps the CLI
// surface honest: nothing here has been verified against a real NAT, so it
// should not be one typo away from being run. The files still compile under
// `go vet ./...`, so they will not silently rot.
//
// To enable, add to the switch in main.go:
//
//	case "relay":   err = cmdRelay(os.Args[2:])
//	case "publish": err = cmdPublish(os.Args[2:])

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"sercon/internal/relay"
)

// cmdRelay runs the rendezvous point both ends connect outbound to.
//
// It is a dumb byte pipe on purpose: it cannot read what passes through it,
// because what passes through it is end-to-end TLS. That is what makes it
// acceptable to put it on a machine neither operator controls.
func cmdRelay(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	listen := fs.String("listen", "", "address to listen on, e.g. 0.0.0.0:8722")
	pairTimeout := fs.Duration("pair-timeout", 30*time.Second, "how long an attacher waits for a console")
	_ = fs.Parse(args)

	if *listen == "" {
		return errors.New("relay: --listen is required")
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("relay: listen %s: %w", *listen, err)
	}
	defer ln.Close()

	srv := &relay.Server{
		PairTimeout: *pairTimeout,
		Logf: func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "sercond: "+format+"\n", a...)
		},
	}

	fmt.Fprintf(os.Stderr, "sercond: relay listening on %s\n", ln.Addr())
	fmt.Fprintf(os.Stderr, "sercond: sessions are end-to-end encrypted; this process only forwards bytes\n")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		ln.Close()
	}()

	if err := srv.ListenAndServe(ln); err != nil {
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
	return nil
}

// cmdPublish exposes a local daemon through a relay.
//
// This is the process that runs on the isolated side. It never accepts an
// inbound connection, so no port forwarding and no firewall rule are needed
// there — it dials out and keeps the connection open.
func cmdPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	socket := fs.String("socket", "", "local daemon socket path")
	relayAddr := fs.String("relay", "", "relay host:port (required)")
	room := fs.String("room", "", "room name for this console (required)")
	slots := fs.Int("slots", 4, "concurrent sessions to serve")
	statusDir := fs.String("state-dir", "", "where to keep the certificate and token")
	_ = fs.Parse(args)

	if *relayAddr == "" {
		return errors.New("publish: --relay is required")
	}
	if *room == "" {
		return errors.New("publish: --room is required")
	}

	sock, err := resolveSocket(*socket)
	if err != nil {
		return err
	}

	// The certificate and the token live next to the socket by default, which
	// is already a per-user private directory on both platforms.
	dir := *statusDir
	if dir == "" {
		dir = filepath.Dir(sock)
	}

	ident, err := relay.LoadIdentity(dir, *room)
	if err != nil {
		return err
	}
	token, err := relay.LoadToken(dir, *room)
	if err != nil {
		return err
	}

	ticket := relay.Ticket{
		Relay:  *relayAddr,
		Room:   *room,
		Token:  token,
		SHA256: ident.Fingerprint,
	}

	pub := &relay.Publisher{
		Relay:    *relayAddr,
		Room:     *room,
		Token:    token,
		Sock:     sock,
		Slots:    *slots,
		Identity: ident,
		Logf: func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "sercond: "+format+"\n", a...)
		},
	}

	// Printed before the first connection attempt: if the relay is unreachable
	// the operator still needs the ticket to hand to the other side, and
	// hunting for it in an error path is no fun.
	printTicket(ticket, ident.Path(), sock)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "sercond: stopping publisher")
		pub.Stop()
	}()

	return pub.Run()
}

func printTicket(t relay.Ticket, certPath, sock string) {
	encoded := t.Encode()

	fmt.Fprintf(os.Stderr, `
sercond: publishing local console to a relay

  relay        %s
  room         %s
  socket       %s
  certificate  %s
  fingerprint  %s

ticket (hand this to the other side, it contains the token and the pin):

  %s

then, from the machine that cannot reach this one:

  sercon ls     --ticket '<ticket>'
  sercon attach --ticket '<ticket>' <port>

`, t.Relay, t.Room, sock, certPath, t.SHA256, encoded)
}
