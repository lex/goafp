package nfsfs

import (
	"context"
	"sync"
	"time"

	"github.com/lex/goafp/internal/afp"
	"github.com/lex/goafp/internal/dsi"
)

// Dialer establishes a fresh, authenticated connection with the target
// volume already open. connManager calls it both for the initial
// connection and to recover after the link drops.
type Dialer func(ctx context.Context) (*dsi.Conn, *afp.Session, *afp.Volume, error)

// dialTimeout bounds a single (re)connection attempt.
const dialTimeout = 30 * time.Second

// connManager owns the live connection generation and transparently
// reconnects when the underlying DSI link dies. A "generation" is one
// established (conn, session, volume) triple; reconnecting mints a new
// generation, which is how open file handles learn their fork IDs are
// stale (the volume pointer changes).
type connManager struct {
	dial      Dialer
	base      context.Context // mount lifetime
	opTimeout time.Duration

	mu   sync.Mutex
	gen  uint64
	conn *dsi.Conn
	sess *afp.Session
	vol  *afp.Volume
}

func newConnManager(base context.Context, dial Dialer, opTimeout time.Duration) (*connManager, error) {
	m := &connManager{dial: dial, base: base, opTimeout: opTimeout}
	ctx, cancel := context.WithTimeout(base, dialTimeout)
	defer cancel()
	conn, sess, vol, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	m.conn, m.sess, m.vol, m.gen = conn, sess, vol, 1
	return m, nil
}

// reconnect rebuilds the connection if the current generation still equals
// failedGen (so concurrent callers that hit the same dead link only
// reconnect once).
func (m *connManager) reconnect(failedGen uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != failedGen {
		return nil // someone already reconnected
	}
	if m.conn != nil {
		m.conn.Close()
	}
	ctx, cancel := context.WithTimeout(m.base, dialTimeout)
	defer cancel()
	conn, sess, vol, err := m.dial(ctx)
	if err != nil {
		m.conn, m.sess, m.vol = nil, nil, nil
		return err
	}
	m.conn, m.sess, m.vol = conn, sess, vol
	m.gen++
	return nil
}

// do runs fn against the current volume with a per-operation timeout. If
// fn fails and the connection is no longer alive, it reconnects once and
// retries. fn receives the op context and the current volume.
func (m *connManager) do(fn func(ctx context.Context, vol *afp.Volume) error) error {
	m.mu.Lock()
	vol, gen, conn := m.vol, m.gen, m.conn
	m.mu.Unlock()

	if vol != nil {
		ctx, cancel := context.WithTimeout(m.base, m.opTimeout)
		err := fn(ctx, vol)
		cancel()
		if err == nil || (conn != nil && conn.IsAlive()) {
			return err
		}
	}

	// The link is dead (or we had no volume): reconnect and retry once.
	if err := m.reconnect(gen); err != nil {
		return err
	}
	m.mu.Lock()
	vol = m.vol
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(m.base, m.opTimeout)
	defer cancel()
	return fn(ctx, vol)
}

func (m *connManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		m.conn.Close()
	}
}

// statFS reports capacity via the current volume (best effort).
func (m *connManager) statFS() (afp.FSInfo, error) {
	var info afp.FSInfo
	err := m.do(func(ctx context.Context, vol *afp.Volume) error {
		var e error
		info, e = vol.StatFS(ctx)
		return e
	})
	return info, err
}
