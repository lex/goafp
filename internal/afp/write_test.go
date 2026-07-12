package afp

import (
	"bytes"
	"sync/atomic"
	"testing"
)

func TestForkWriteAt(t *testing.T) {
	fork, mock := openMockFork(t, nil, 0)
	fork.chunkSize = 1024
	fork.maxParallel = 4

	data := patternData(10000)
	n, err := fork.WriteAt(testCtxFork(t), data, 0)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if n != len(data) {
		t.Errorf("wrote %d bytes, want %d", n, len(data))
	}
	if got := mock.contents(); !bytes.Equal(got, data) {
		t.Errorf("server received %d bytes, content match=%v", len(got), bytes.Equal(got, data))
	}
	// 10000 bytes in 1024-byte chunks must be several writes.
	if got := atomic.LoadInt64(&mock.writes); got < 9 {
		t.Errorf("only %d write requests, expected coalesced chunking", got)
	}
}

func TestForkWriteChunkSizeCappedByQuantum(t *testing.T) {
	fork, _ := openMockFork(t, nil, 0)
	// The mock's DSI conn never negotiated a quantum, so serverQuantum
	// is 0 and the configured size is used as-is.
	fork.chunkSize = 200000
	if got := fork.writeChunkSize(); got != 200000 {
		t.Errorf("writeChunkSize = %d, want 200000 when quantum unknown", got)
	}
}

func TestForkReadFrom(t *testing.T) {
	fork, mock := openMockFork(t, nil, 0)
	fork.chunkSize = 1024
	fork.maxParallel = 3

	data := patternData(7000)
	n, err := fork.ReadFromContext(testCtxFork(t), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("ReadFrom returned %d, want %d", n, len(data))
	}
	if got := mock.contents(); !bytes.Equal(got, data) {
		t.Errorf("round-tripped content mismatch: got %d bytes", len(got))
	}
}

// TestForkWriteAtIsPipelined gates the mock on exactly the number of
// chunks one WriteAt should emit; a serial writer would deadlock.
func TestForkWriteAtIsPipelined(t *testing.T) {
	fork, mock := openMockFork(t, nil, 0)
	fork.chunkSize = 1024
	fork.maxParallel = 4

	// Gate writes: hold until all 4 arrive. We reuse the read gate
	// machinery by counting writes here through a manual barrier.
	const chunks = 4
	mock.gate = chunks
	mock.gateWrites = true

	data := patternData(4096)
	ctx := testCtxFork(t)
	n, err := fork.WriteAt(ctx, data, 0)
	if err != nil {
		t.Fatalf("WriteAt: %v (a serial writer would deadlock)", err)
	}
	if n != len(data) {
		t.Errorf("wrote %d, want %d", n, len(data))
	}
	if got := atomic.LoadInt64(&mock.maxHeld); got != chunks {
		t.Errorf("max concurrent in-flight writes = %d, want %d", got, chunks)
	}
	if got := mock.contents(); !bytes.Equal(got, data) {
		t.Errorf("content mismatch after pipelined write")
	}
}
