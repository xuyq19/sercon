// Package relay carries a serial console across a network boundary that
// neither end can cross directly.
//
// # Status
//
// Implemented but NOT wired up, and not verified against a real NAT. The CLI
// entry points exist in cmd/sercond/relay.go but are deliberately left out of
// the command switch, so nothing here is reachable by an operator yet. The
// package still compiles under `go vet ./...` so it will not silently rot.
//
// # Why a relay instead of hole punching
//
// The usual answer to "the two ends are behind NAT" is a signalling server plus
// STUN plus hole punching, with a relay kept as a fallback for the cases
// punching cannot handle — symmetric NAT, and corporate networks that drop UDP
// outright. That whole apparatus exists to avoid paying for relayed bandwidth.
//
// A serial console runs at 115200 baud, or about 11 KB/s, and it is a console:
// it is idle most of the time. The bandwidth argument does not apply, so
// punching would buy latency and nothing else, while failing on exactly the
// networks this is meant to traverse. A relay alone works everywhere.
//
// # Where the encryption lives
//
// The relay is a dumb byte pipe. It never sees the protocol, and it never sees
// console output. Both ends run TLS through the relayed connection, with the
// publisher as the server and a pinned certificate on the client. That matters
// because a serial console prints login prompts: whatever is typed at one is
// typed in the clear as far as the console line is concerned, and a relay
// operator able to read the stream would be reading credentials.
package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Role distinguishes the two ends of a pairing.
type Role string

const (
	// RolePublish is the end that owns the console and serves TLS.
	RolePublish Role = "publish"
	// RoleAttach is the end that connects and speaks the protocol.
	RoleAttach Role = "attach"
)

// hello is the first line each peer sends to the relay.
type hello struct {
	Room  string `json:"room"`
	Token string `json:"token"`
	Role  Role   `json:"role"`
	// Slots is how many concurrent sessions a publisher wants to serve. The
	// relay answers one publisher connection per slot, which is what keeps
	// concurrent clients working without a multiplexing layer.
	Slots int `json:"slots,omitempty"`
}

// reply is the relay's answer, and later its state notifications.
type reply struct {
	OK    bool   `json:"ok"`
	State string `json:"state,omitempty"`
	Error string `json:"error,omitempty"`
}

// Relay states a peer can be told about.
const (
	// StateWaiting means the peer authenticated and is queued. A publisher in
	// this state is idle; an attacher in this state is waiting for one.
	StateWaiting = "waiting"
	// StatePaired means the peer has a partner and the bytes that follow are
	// the session.
	StatePaired = "paired"
)

// Ticket is everything a client needs to reach one console through a relay.
//
// It is deliberately a single opaque string: three separate values to copy
// correctly is three chances to get one wrong, and the fingerprint in
// particular is the kind of thing that gets dropped when it is inconvenient.
type Ticket struct {
	// Relay is the relay's host:port.
	Relay string `json:"relay"`
	// Room names the console. Also the room a publisher registers under.
	Room string `json:"room"`
	// Token authenticates both ends to the relay.
	Token string `json:"token"`
	// SHA256 pins the publisher's certificate, as hex.
	SHA256 string `json:"sha256"`
}

// Encode renders the ticket as one copy-pasteable argument.
func (t Ticket) Encode() string {
	raw, err := json.Marshal(t)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeTicket parses an encoded ticket. A bare host:port is also accepted, for
// the case where the relay is on an open network and the operator has set the
// security aside; the caller decides whether to allow that.
func DecodeTicket(s string) (Ticket, error) {
	s = strings.TrimSpace(s)

	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		var t Ticket
		if err := json.Unmarshal(raw, &t); err == nil && t.Relay != "" {
			return t, nil
		}
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return Ticket{Relay: s}, nil
	}
	return Ticket{}, fmt.Errorf("relay: %q is not a valid ticket", s)
}

// Complete reports whether the ticket carries everything needed for a secure
// connection. Without a token and a fingerprint the session would be neither
// authenticated nor encrypted, which the caller is expected to refuse.
func (t Ticket) Complete() bool {
	return t.Relay != "" && t.Room != "" && t.Token != "" && t.SHA256 != ""
}

// tlsConfig builds the pinning client configuration.
func (t Ticket) tlsConfig() *tls.Config {
	want := strings.ToLower(strings.TrimSpace(t.SHA256))
	return &tls.Config{
		// Verification is replaced rather than skipped: the publisher's
		// certificate is self-signed, so there is no chain to walk, and the
		// fingerprint from the ticket is a stronger statement than a CA would
		// be for a single-purpose endpoint.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("relay: publisher presented no certificate")
			}
			got := Fingerprint(rawCerts[0])
			if !strings.EqualFold(got, want) {
				return fmt.Errorf("relay: publisher fingerprint mismatch\n  want %s\n  got  %s", want, got)
			}
			return nil
		},
	}
}

// LoadToken reads the room's shared secret, creating it on first use.
//
// Persisted for the same reason the certificate is: a token that changed on
// every restart would invalidate every ticket already handed out, and the
// operator would be re-copying it constantly instead of getting on with work.
func LoadToken(dir, room string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("relay: %w", err)
	}
	path := filepath.Join(dir, "relay-"+sanitize(room)+".token")

	if raw, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(raw)); len(tok) >= minTokenLen {
			return tok, nil
		}
	}

	// 32 bytes of entropy, base64url. Far beyond guessing, and short enough to
	// survive being pasted into a chat message.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("relay: generate token: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(buf)

	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("relay: write %s: %w", path, err)
	}
	return tok, nil
}

// Fingerprint is the hex SHA-256 of a DER certificate, the form the ticket
// carries and the form an operator can compare by eye.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Identity is the publisher's TLS certificate, persisted so that the
// fingerprint in a ticket stays valid across restarts. Regenerating it every
// launch would invalidate every ticket already handed out.
type Identity struct {
	Cert        tls.Certificate
	Fingerprint string
	path        string
}

// LoadIdentity reads the publisher certificate for a room, creating it on first
// use. dir should be a per-user private directory; the key is written 0600.
func LoadIdentity(dir, room string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}

	base := filepath.Join(dir, "relay-"+sanitize(room))
	certPath, keyPath := base+".crt", base+".key"

	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return newIdentity(cert, certPath)
	} else if !os.IsNotExist(err) {
		// A corrupt or mismatched pair is worth reporting rather than silently
		// replacing: the operator may have copied the files deliberately.
		if !errorsIsNotExist(err) {
			return nil, fmt.Errorf("relay: load identity %s: %w", certPath, err)
		}
	}

	cert, err := generateIdentity()
	if err != nil {
		return nil, err
	}
	if err := writeIdentity(certPath, keyPath, cert); err != nil {
		return nil, err
	}
	return newIdentity(cert, certPath)
}

func newIdentity(cert tls.Certificate, path string) (*Identity, error) {
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("relay: certificate file %s contains no certificate", path)
	}
	return &Identity{
		Cert:        cert,
		Fingerprint: Fingerprint(cert.Certificate[0]),
		path:        path,
	}, nil
}

// Path is where the certificate lives, for display.
func (i *Identity) Path() string { return i.path }

// TLSConfig serves TLS with this identity.
func (i *Identity) TLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{i.Cert},
		MinVersion:   tls.VersionTLS13,
	}
}

func generateIdentity() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("relay: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("relay: serial: %w", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "sercond-relay-publisher"},
		// Backdated slightly so a clock a few seconds out does not reject it.
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("relay: create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("relay: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return tls.X509KeyPair(certPEM, keyPEM)
}

func writeIdentity(certPath, keyPath string, cert tls.Certificate) error {
	var certPEM, keyPEM []byte
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if len(cert.PrivateKey.(*ecdsa.PrivateKey).D.Bytes()) > 0 {
		der, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
		if err != nil {
			return fmt.Errorf("relay: marshal key: %w", err)
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	}

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("relay: write %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("relay: write %s: %w", keyPath, err)
	}
	return nil
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

// sanitize keeps a room name usable as part of a filename.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "room"
	}
	return b.String()
}
