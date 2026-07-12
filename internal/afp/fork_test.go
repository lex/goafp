package afp

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lex/goafp/internal/dsi"
)

// forkMock is a minimal AFP server that serves a single in-memory fork.
// With gate > 0 it holds FPReadExt replies until gate of them have
// arrived, then answers all at once in reverse order — a serial reader
// would deadlock, so a passing test proves ReadAt pipelines.
type forkMock struct {
	t    *testing.T
	nc   net.Conn
	data []byte

	gate    int
	reads   int64 // total FPReadExt requests observed
	maxHeld int64 // high-water mark of concurrent in-flight reads

	mu   sync.Mutex
	held []heldRead
}

type heldRead struct {
	reqID  uint16
	offset int64
	count  int
}

func startForkMock(t *testing.T, nc net.Conn, data []byte, gate int) *forkMock {
	t.Helper()
	m := &forkMock{t: t, nc: nc, data: data, gate: gate}
	go m.loop()
	return m
}

func (m *forkMock) loop() {
	for {
		var hb [dsi.HeaderSize]byte
		if _, err := io.ReadFull(m.nc, hb[:]); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(hb[8:12])
		payload := make([]byte, length)
		if _, err := io.ReadFull(m.nc, payload); err != nil {
			return
		}
		if hb[0] != 0 || hb[1] != dsi.CmdCommand || len(payload) == 0 {
			continue
		}
		reqID := binary.BigEndian.Uint16(hb[2:4])
		m.handle(reqID, payload)
	}
}

func (m *forkMock) send(reqID uint16, code Result, payload []byte) {
	var hb [dsi.HeaderSize]byte
	hb[0] = 1
	hb[1] = dsi.CmdCommand
	binary.BigEndian.PutUint16(hb[2:4], reqID)
	binary.BigEndian.PutUint32(hb[4:8], uint32(int32(code)))
	binary.BigEndian.PutUint32(hb[8:12], uint32(len(payload)))
	m.nc.Write(append(hb[:], payload...))
}

func (m *forkMock) handle(reqID uint16, payload []byte) {
	switch payload[0] {
	case cmdOpenFork:
		var w builder
		w.u16(kFPExtDataForkLenBit) // echo bitmap
		w.u16(7)                    // fork id
		w.u64(uint64(len(m.data)))  // fork length
		m.send(reqID, ResNoErr, w.b)
	case cmdCloseFork:
		m.send(reqID, ResNoErr, nil)
	case cmdReadExt:
		atomic.AddInt64(&m.reads, 1)
		offset := int64(binary.BigEndian.Uint64(payload[4:12]))
		count := int(binary.BigEndian.Uint64(payload[12:20]))
		if m.gate > 0 {
			m.gateRead(reqID, offset, count)
			return
		}
		data, code := m.slice(offset, count)
		m.send(reqID, code, data)
	default:
		m.send(reqID, ResCallNotSupported, nil)
	}
}

func (m *forkMock) gateRead(reqID uint16, offset int64, count int) {
	m.mu.Lock()
	m.held = append(m.held, heldRead{reqID, offset, count})
	held := int64(len(m.held))
	if held > atomic.LoadInt64(&m.maxHeld) {
		atomic.StoreInt64(&m.maxHeld, held)
	}
	ready := len(m.held) == m.gate
	batch := m.held
	if ready {
		m.held = nil
	}
	m.mu.Unlock()

	if !ready {
		return
	}
	// Reply in reverse arrival order to also exercise reassembly.
	for i := len(batch) - 1; i >= 0; i-- {
		h := batch[i]
		data, code := m.slice(h.offset, h.count)
		m.send(h.reqID, code, data)
	}
}

func (m *forkMock) slice(offset int64, count int) ([]byte, Result) {
	if offset >= int64(len(m.data)) {
		return nil, ResEOF
	}
	end := offset + int64(count)
	if end >= int64(len(m.data)) {
		return m.data[offset:], ResEOF
	}
	return m.data[offset:end], ResNoErr
}

func openMockFork(t *testing.T, data []byte, gate int) (*Fork, *forkMock) {
	t.Helper()
	clientEnd, serverEnd := net.Pipe()
	t.Cleanup(func() { serverEnd.Close() })
	mock := startForkMock(t, serverEnd, data, gate)
	conn := dsi.NewConn(clientEnd)
	t.Cleanup(func() { conn.Close() })

	vol := &Volume{s: NewSession(conn), ID: 1}
	fork, err := vol.OpenFork(testCtxFork(t), RootDirID, "file")
	if err != nil {
		t.Fatalf("OpenFork: %v", err)
	}
	return fork, mock
}

func testCtxFork(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func patternData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251) // prime stride so misordering is visible
	}
	return b
}

func TestForkReadAtWholeFile(t *testing.T) {
	data := patternData(10000)
	fork, _ := openMockFork(t, data, 0)
	if fork.Size != uint64(len(data)) {
		t.Errorf("fork size = %d, want %d", fork.Size, len(data))
	}
	fork.chunkSize = 1024
	fork.maxParallel = 4

	buf := make([]byte, len(data))
	n, err := fork.ReadAt(testCtxFork(t), buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(data) || !bytes.Equal(buf, data) {
		t.Errorf("ReadAt returned %d bytes, content match=%v", n, bytes.Equal(buf, data))
	}
}

func TestForkReadAtEOF(t *testing.T) {
	data := patternData(3000)
	fork, _ := openMockFork(t, data, 0)
	fork.chunkSize = 1024
	fork.maxParallel = 4

	// Ask for more than exists.
	buf := make([]byte, 8192)
	n, err := fork.ReadAt(testCtxFork(t), buf, 0)
	if err != io.EOF {
		t.Fatalf("ReadAt err = %v, want io.EOF", err)
	}
	if n != len(data) || !bytes.Equal(buf[:n], data) {
		t.Errorf("short read n=%d, want %d", n, len(data))
	}
}

// TestForkReadAtIsPipelined gates the server on exactly the number of
// chunks a single ReadAt should issue. If ReadAt read serially, only one
// request would arrive, the server would never flush, and the test would
// hit its context deadline.
func TestForkReadAtIsPipelined(t *testing.T) {
	data := patternData(4096)
	const chunk = 1024
	const chunks = 4 // 4096 / 1024

	fork, mock := openMockFork(t, data, chunks)
	fork.chunkSize = chunk
	fork.maxParallel = chunks

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	buf := make([]byte, len(data))
	n, err := fork.ReadAt(ctx, buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v (a serial reader would deadlock here)", err)
	}
	if n != len(data) || !bytes.Equal(buf, data) {
		t.Errorf("pipelined read corrupted data: n=%d match=%v", n, bytes.Equal(buf, data))
	}
	if got := atomic.LoadInt64(&mock.maxHeld); got != chunks {
		t.Errorf("max concurrent in-flight reads = %d, want %d", got, chunks)
	}
}

func TestForkWriteTo(t *testing.T) {
	data := patternData(5000)
	fork, mock := openMockFork(t, data, 0)
	fork.chunkSize = 1024
	fork.maxParallel = 3

	var buf bytes.Buffer
	n, err := fork.WriteToContext(testCtxFork(t), &buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != int64(len(data)) || !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("WriteTo wrote %d bytes, content match=%v", n, bytes.Equal(buf.Bytes(), data))
	}
	// Sanity: streaming a 5000-byte file in 1024-byte chunks needs
	// several reads, confirming we actually chunked.
	if got := atomic.LoadInt64(&mock.reads); got < 5 {
		t.Errorf("only %d read requests, expected chunked reads", got)
	}
}
