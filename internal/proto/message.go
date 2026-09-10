package proto

// Control operation names.
const (
	OpHello    = "hello"
	OpWelcome  = "welcome"
	OpList     = "list"
	OpPorts    = "ports"
	OpOpen     = "open"
	OpOpened   = "opened"
	OpClose    = "close"
	OpClosed   = "closed"
	OpPing     = "ping"
	OpPong     = "pong"
	OpBreak    = "break"
	OpStatus   = "status"
	OpStat     = "stat"
	OpNotice   = "notice"
	OpError    = "error"
	OpBye      = "bye"
	OpShutdown = "shutdown"
)

// Message is a control frame payload. One flat struct keeps the wire format
// readable in tcpdump and avoids a registry of per-op types for a protocol
// this small; unused fields are omitted.
type Message struct {
	Op string `json:"op"`
	V  int    `json:"v,omitempty"`

	// Identity, established during the handshake.
	User   string `json:"user,omitempty"`
	Client string `json:"client,omitempty"`
	Server string `json:"server,omitempty"`

	// Free-form text for notice/error/closed.
	Text string `json:"text,omitempty"`
	// Machine-readable error class.
	Code string `json:"code,omitempty"`

	// Port addressing and session role.
	Port      string     `json:"port,omitempty"`
	Ports     []PortInfo `json:"ports,omitempty"`
	Baud      int        `json:"baud,omitempty"`
	Observe   bool       `json:"observe,omitempty"`
	Writable  bool       `json:"writable,omitempty"`
	Owner     string     `json:"owner,omitempty"`
	Observers int        `json:"observers,omitempty"`

	// Session bookkeeping surfaced back to the client.
	Log     string `json:"log,omitempty"`
	Since   string `json:"since,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Offline bool   `json:"offline,omitempty"`
	Err     string `json:"err,omitempty"`
}

// PortInfo describes one serial port as seen by the daemon.
type PortInfo struct {
	// Ref is the stable reference clients use, derived from /dev/serial/by-id.
	Ref string `json:"ref"`
	// Dev is the device path currently backing Ref.
	Dev string `json:"dev"`
	// Desc is a human label, either from config or the by-id name.
	Desc string `json:"desc,omitempty"`

	Baud      int    `json:"baud"`
	Online    bool   `json:"online"`
	Owner     string `json:"owner,omitempty"`
	Observers int    `json:"observers,omitempty"`

	Log     string `json:"log,omitempty"`
	LastErr string `json:"last_err,omitempty"`
}
