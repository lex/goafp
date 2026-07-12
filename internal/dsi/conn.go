package dsi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// TickleInterval is how often the client sends a DSITickle to keep the
// connection alive. AFP servers drop peers that fall silent, so an idle
// mount needs these. It is a variable so tests can shorten it.
var TickleInterval = 30 * time.Second

// maxPayload bounds how large a payload we accept from a server. Real
// replies are limited by the negotiated quantum (typically <= 1 MB); this
// guard keeps a broken or hostile server from making us allocate wildly.
const maxPayload = 16 << 20

// attentionQuantum is the client attention quantum advertised during
// DSIOpenSession.
const attentionQuantum = 1024

// DSIOpenSession option types.
const (
	optServerQuantum    uint8 = 0x00
	optAttentionQuantum uint8 = 0x01
)

// Reply is a server response to a single DSI request.
type Reply struct {
	Header  Header
	Payload []byte
}

// Conn is a DSI session over a TCP connection. It is safe for concurrent
// use: requests from multiple goroutines are pipelined on the wire and
// replies are routed back to the caller by request ID.
type Conn struct {
	nc net.Conn

	writeMu sync.Mutex // serializes writes to nc

	mu       sync.Mutex // guards nextID, pending, closeErr
	nextID   uint16
	pending  map[uint16]chan Reply
	closeErr error

	done chan struct{} // closed when the connection dies

	// ServerQuantum is the server request quantum from DSIOpenSession:
	// the largest request payload the server accepts (caps write sizes).
	ServerQuantum uint32
}

// Dial connects to an AFP server. addr may omit the port, in which case
// the standard AFP port 548 is used.
func Dial(ctx context.Context, addr string) (*Conn, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "548")
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return NewConn(nc), nil
}

// NewConn wraps an established transport in a DSI session and starts its
// reader. Exposed so tests and alternate transports can inject their own
// net.Conn.
func NewConn(nc net.Conn) *Conn {
	c := &Conn{
		nc:      nc,
		pending: make(map[uint16]chan Reply),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Request sends a DSI request and waits for the matching reply. Multiple
// callers may issue requests concurrently; they are answered as the server
// replies, in any order.
func (c *Conn) Request(ctx context.Context, cmd uint8, payload []byte) (Reply, error) {
	return c.request(ctx, cmd, 0, payload)
}

// RequestWrite sends a DSIWrite request: dataOffset is the offset within
// payload at which bulk write data starts.
func (c *Conn) RequestWrite(ctx context.Context, dataOffset uint32, payload []byte) (Reply, error) {
	return c.request(ctx, CmdWrite, dataOffset, payload)
}

func (c *Conn) request(ctx context.Context, cmd uint8, errOrOffset uint32, payload []byte) (Reply, error) {
	ch := make(chan Reply, 1)
	id, err := c.register(ch)
	if err != nil {
		return Reply{}, err
	}
	defer c.unregister(id)

	h := Header{
		Flags:     flagRequest,
		Command:   cmd,
		RequestID: id,
		ErrCode:   errOrOffset,
		Length:    uint32(len(payload)),
	}
	if err := c.send(h, payload); err != nil {
		return Reply{}, err
	}

	select {
	case r := <-ch:
		return r, nil
	case <-c.done:
		return Reply{}, c.err()
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
}

// OpenSession performs the DSIOpenSession exchange and records the
// server's request quantum.
func (c *Conn) OpenSession(ctx context.Context) error {
	payload := make([]byte, 6)
	payload[0] = optAttentionQuantum
	payload[1] = 4
	binary.BigEndian.PutUint32(payload[2:6], attentionQuantum)

	r, err := c.Request(ctx, CmdOpenSession, payload)
	if err != nil {
		return err
	}

	// Now that a session exists, keep it alive with periodic tickles.
	// Read the interval here (on the caller's goroutine) rather than in
	// the loop, so tests can vary it without racing the ticker goroutine.
	go c.tickleLoop(TickleInterval)

	// The reply payload is a sequence of (type, length, value) options.
	for i := 0; i+2 <= len(r.Payload); {
		typ, n := r.Payload[i], int(r.Payload[i+1])
		i += 2
		if i+n > len(r.Payload) {
			return fmt.Errorf("dsi: malformed OpenSession option %#x", typ)
		}
		if typ == optServerQuantum && n == 4 {
			c.ServerQuantum = binary.BigEndian.Uint32(r.Payload[i : i+4])
		}
		i += n
	}
	return nil
}

// GetStatus performs DSIGetStatus and returns the raw FPGetSrvrInfo reply
// block. It works before OpenSession, so it needs no authentication.
func (c *Conn) GetStatus(ctx context.Context) ([]byte, error) {
	r, err := c.Request(ctx, CmdGetStatus, nil)
	if err != nil {
		return nil, err
	}
	return r.Payload, nil
}

// Close sends DSICloseSession (best effort) and tears down the connection.
func (c *Conn) Close() error {
	h := Header{Flags: flagRequest, Command: CmdCloseSession, RequestID: c.newID()}
	_ = c.send(h, nil)
	err := c.nc.Close()
	c.fail(net.ErrClosed)
	return err
}

func (c *Conn) send(h Header, payload []byte) error {
	buf := make([]byte, HeaderSize+len(payload))
	hb := h.encode()
	copy(buf, hb[:])
	copy(buf[HeaderSize:], payload)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.nc.Write(buf); err != nil {
		return err
	}
	return nil
}

func (c *Conn) register(ch chan Reply) (uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr != nil {
		return 0, c.closeErr
	}
	for {
		c.nextID++
		if _, taken := c.pending[c.nextID]; !taken {
			break
		}
	}
	c.pending[c.nextID] = ch
	return c.nextID, nil
}

func (c *Conn) unregister(id uint16) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Conn) newID() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

func (c *Conn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// IsAlive reports whether the connection is still usable (no read error or
// close has torn it down).
func (c *Conn) IsAlive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr == nil
}

// tickleLoop sends a DSITickle at the given interval until the connection
// dies.
func (c *Conn) tickleLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			h := Header{Flags: flagRequest, Command: CmdTickle, RequestID: c.newID()}
			if err := c.send(h, nil); err != nil {
				return
			}
		}
	}
}

// fail marks the connection dead and wakes every waiter.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr != nil {
		return
	}
	c.closeErr = err
	close(c.done)
}

func (c *Conn) readLoop() {
	for {
		var hb [HeaderSize]byte
		if _, err := io.ReadFull(c.nc, hb[:]); err != nil {
			c.fail(err)
			return
		}
		h := decodeHeader(hb)
		if h.Length > maxPayload {
			c.fail(fmt.Errorf("dsi: server claims %d byte payload", h.Length))
			c.nc.Close()
			return
		}
		payload := make([]byte, h.Length)
		if _, err := io.ReadFull(c.nc, payload); err != nil {
			c.fail(err)
			return
		}

		if h.Flags == flagReply {
			c.mu.Lock()
			ch := c.pending[h.RequestID]
			c.mu.Unlock()
			if ch != nil {
				ch <- Reply{Header: h, Payload: payload}
			}
			// An unknown request ID means the caller gave up
			// (context cancelled); drop the reply.
			continue
		}

		// Server-initiated request.
		switch h.Command {
		case CmdTickle:
			// Keepalive. No reply is required; we send our own
			// tickles. TODO: periodic client-side tickles.
		case CmdAttention:
			// TODO: parse attention flags (shutdown notices etc).
		case CmdCloseSession:
			c.fail(errors.New("dsi: server closed the session"))
			c.nc.Close()
			return
		}
	}
}
