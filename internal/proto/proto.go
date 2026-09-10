// Package proto implements the framed wire protocol spoken between the local
// sercon client and the sercond capture daemon running on the jump host.
//
// Frame layout:
//
//	1 byte   frame type
//	4 bytes  payload length, big-endian
//	n bytes  payload
//
// TypeData frames carry raw serial bytes and are never inspected. TypeCtrl
// frames carry a JSON control message. Because all terminal control characters
// travel inside length-prefixed payloads, the protocol is fully transparent to
// whatever the serial line produces.
package proto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Frame types.
const (
	TypeData byte = 0x01
	TypeCtrl byte = 0x02
)

// Version is the control protocol version. Both ends compare it during the
// hello/welcome handshake so a stale binary fails loudly instead of misparsing.
const Version = 1

// MaxPayload caps a single frame. Serial traffic never comes close to this;
// exceeding it means corruption or a hostile peer.
const MaxPayload = 1 << 20

var (
	ErrFrameTooLarge = errors.New("proto: frame payload exceeds limit")
	ErrBadType       = errors.New("proto: unknown frame type")
)

// Frame is one protocol frame.
type Frame struct {
	Type    byte
	Payload []byte
}

// Reader decodes frames. It is not safe for concurrent use.
type Reader struct {
	r   *bufio.Reader
	hdr [5]byte
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next blocks until a full frame is available.
func (rd *Reader) Next() (Frame, error) {
	if _, err := io.ReadFull(rd.r, rd.hdr[:]); err != nil {
		return Frame{}, err
	}
	t := rd.hdr[0]
	if t != TypeData && t != TypeCtrl {
		return Frame{}, fmt.Errorf("%w: 0x%02x", ErrBadType, t)
	}
	n := binary.BigEndian.Uint32(rd.hdr[1:])
	if n > MaxPayload {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(rd.r, p); err != nil {
		return Frame{}, err
	}
	return Frame{Type: t, Payload: p}, nil
}

// Writer encodes frames. Safe for concurrent use; frames are never interleaved.
//
// Every frame is flushed immediately. Buffering across frames would deadlock a
// duplex console: the writer would sit on bytes while the reader waits for a
// reply that never leaves the buffer.
type Writer struct {
	mu  sync.Mutex
	w   *bufio.Writer
	hdr [5]byte
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 64*1024)}
}

func (wr *Writer) Write(t byte, p []byte) error {
	if len(p) > MaxPayload {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(p))
	}
	wr.mu.Lock()
	defer wr.mu.Unlock()

	wr.hdr[0] = t
	binary.BigEndian.PutUint32(wr.hdr[1:], uint32(len(p)))
	if _, err := wr.w.Write(wr.hdr[:]); err != nil {
		return err
	}
	if len(p) > 0 {
		if _, err := wr.w.Write(p); err != nil {
			return err
		}
	}
	return wr.w.Flush()
}

// Data sends raw serial bytes.
func (wr *Writer) Data(p []byte) error { return wr.Write(TypeData, p) }

// Ctrl sends a JSON control message.
func (wr *Writer) Ctrl(m *Message) error {
	p, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return wr.Write(TypeCtrl, p)
}
