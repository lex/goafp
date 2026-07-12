package dsi

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestHeaderRoundTrip(t *testing.T) {
	in := Header{
		Flags:     flagReply,
		Command:   CmdCommand,
		RequestID: 0xBEEF,
		ErrCode:   0xDEADBEEF,
		Length:    12345,
		Reserved:  7,
	}
	out := decodeHeader(in.encode())
	if out != in {
		t.Errorf("round trip mismatch: got %+v, want %+v", out, in)
	}
}

// mockServer reads DSI requests off the wire and passes them to handle,
// which is responsible for writing any replies.
func mockServer(t *testing.T, nc net.Conn, handle func(h Header, payload []byte, w io.Writer)) {
	t.Helper()
	go func() {
		for {
			var hb [HeaderSize]byte
			if _, err := io.ReadFull(nc, hb[:]); err != nil {
				return
			}
			h := decodeHeader(hb)
			payload := make([]byte, h.Length)
			if _, err := io.ReadFull(nc, payload); err != nil {
				return
			}
			handle(h, payload, nc)
		}
	}()
}

func writeReply(w io.Writer, h Header, payload []byte) {
	rh := Header{
		Flags:     flagReply,
		Command:   h.Command,
		RequestID: h.RequestID,
		Length:    uint32(len(payload)),
	}
	hb := rh.encode()
	buf := append(hb[:], payload...)
	w.Write(buf)
}

func TestRequestReply(t *testing.T) {
	clientEnd, serverEnd := net.Pipe()
	defer serverEnd.Close()

	mockServer(t, serverEnd, func(h Header, payload []byte, w io.Writer) {
		writeReply(w, h, append([]byte("re:"), payload...))
	})

	c := NewConn(clientEnd)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, err := c.Request(ctx, CmdCommand, []byte("hello"))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got, want := string(r.Payload), "re:hello"; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
}

// TestPipelinedOutOfOrder proves the architectural property the C
// implementation lacked: several requests in flight at once, with replies
// arriving out of order and still reaching the right callers.
func TestPipelinedOutOfOrder(t *testing.T) {
	clientEnd, serverEnd := net.Pipe()
	defer serverEnd.Close()

	const n = 8
	var mu sync.Mutex
	var held []struct {
		h       Header
		payload []byte
	}

	// Hold all n requests, then answer them in reverse arrival order,
	// with a stray server tickle mixed in.
	mockServer(t, serverEnd, func(h Header, payload []byte, w io.Writer) {
		mu.Lock()
		held = append(held, struct {
			h       Header
			payload []byte
		}{h, append([]byte(nil), payload...)})
		ready := len(held) == n
		mu.Unlock()
		if !ready {
			return
		}
		tickle := Header{Flags: flagRequest, Command: CmdTickle}
		hb := tickle.encode()
		w.Write(hb[:])
		mu.Lock()
		defer mu.Unlock()
		for i := len(held) - 1; i >= 0; i-- {
			writeReply(w, held[i].h, append([]byte("re:"), held[i].payload...))
		}
	})

	c := NewConn(clientEnd)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := fmt.Sprintf("req-%d", i)
			r, err := c.Request(ctx, CmdCommand, []byte(msg))
			if err != nil {
				errs[i] = err
				return
			}
			if got, want := string(r.Payload), "re:"+msg; got != want {
				errs[i] = fmt.Errorf("payload = %q, want %q", got, want)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d: %v", i, err)
		}
	}
}

func TestOpenSessionParsesServerQuantum(t *testing.T) {
	clientEnd, serverEnd := net.Pipe()
	defer serverEnd.Close()

	mockServer(t, serverEnd, func(h Header, payload []byte, w io.Writer) {
		if h.Command != CmdOpenSession {
			return // ignore the CloseSession sent by c.Close
		}
		// Reply with a server request quantum of 1 MB.
		reply := []byte{optServerQuantum, 4, 0x00, 0x10, 0x00, 0x00}
		writeReply(w, h, reply)
	})

	c := NewConn(clientEnd)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.OpenSession(ctx); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if got, want := c.ServerQuantum, uint32(1<<20); got != want {
		t.Errorf("ServerQuantum = %d, want %d", got, want)
	}
}

func TestServerCloseFailsPendingRequests(t *testing.T) {
	clientEnd, serverEnd := net.Pipe()

	mockServer(t, serverEnd, func(h Header, payload []byte, w io.Writer) {
		serverEnd.Close()
	})

	c := NewConn(clientEnd)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Request(ctx, CmdCommand, []byte("x")); err == nil {
		t.Error("Request succeeded after server hangup, want error")
	}
}
