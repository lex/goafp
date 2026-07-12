//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lex/goafp/internal/nfsfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// TestNFSBridgeEndToEnd serves the netatalk-backed AFP volume over NFS on a
// local socket and drives it with an in-process NFS client — exercising the
// full AFP -> billy -> NFS stack without needing sudo to mount.
func TestNFSBridgeEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := login(t)
	defer sess.Logout(context.Background())

	vol, err := sess.OpenVolume(ctx, cfg(t, "GOAFP_TEST_VOLUME"))
	if err != nil {
		t.Fatalf("OpenVolume: %v", err)
	}
	defer vol.Close(context.Background())

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go nfsfs.Serve(l, nfsfs.New(ctx, vol), 1024)
	defer l.Close()

	// go-nfs multiplexes MOUNT and NFS on the single listener port with no
	// portmapper, so dial it directly rather than via DialMount (which
	// would look for portmap on :111).
	client, err := dialTCPWithRetry(t, l.Addr().(*net.TCPAddr).String())
	if err != nil {
		t.Fatalf("dial nfs: %v", err)
	}
	mount := &nfsclient.Mount{Client: client}
	defer mount.Close()

	target, err := mount.Mount("/", rpc.AuthNull)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer target.Close()

	// The share already contains hello.txt (seeded by run.sh).
	entries, err := target.ReadDirPlus("/")
	if err != nil {
		t.Fatalf("ReadDirPlus: %v", err)
	}
	if !containsEntry(entries, "hello.txt") {
		t.Errorf("hello.txt not listed over NFS: %v", entryNames(entries))
	}

	// Read hello.txt through NFS.
	rf, err := target.Open("/hello.txt")
	if err != nil {
		t.Fatalf("Open hello.txt: %v", err)
	}
	got, err := io.ReadAll(rf)
	rf.Close()
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(got) != "hello, world!\n" {
		t.Errorf("hello.txt over NFS = %q", got)
	}

	// Create a directory and write a file into it, then read it back.
	if _, err := target.Mkdir("/nfsdir", 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	t.Cleanup(func() {
		target.Remove("/nfsdir/greeting.txt")
		target.Remove("/nfsdir")
	})

	want := bytes.Repeat([]byte("goafp over nfs\n"), 5000) // multi-chunk
	wf, err := target.OpenFile("/nfsdir/greeting.txt", 0644)
	if err != nil {
		t.Fatalf("OpenFile for write: %v", err)
	}
	if _, err := wf.Write(want); err != nil {
		t.Fatalf("write over NFS: %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("close after write: %v", err)
	}

	rf2, err := target.Open("/nfsdir/greeting.txt")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got2, err := io.ReadAll(rf2)
	rf2.Close()
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if !bytes.Equal(got2, want) {
		t.Errorf("NFS write/read round trip mismatch: got %d bytes, want %d", len(got2), len(want))
	}
}

func dialTCPWithRetry(t *testing.T, addr string) (*rpc.Client, error) {
	t.Helper()
	var lastErr error
	for i := 0; i < 20; i++ {
		c, err := rpc.DialTCP("tcp", addr, false)
		if err == nil {
			return c, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, lastErr
}

func containsEntry(entries []*nfsclient.EntryPlus, name string) bool {
	for _, e := range entries {
		if e.Name() == name {
			return true
		}
	}
	return false
}

func entryNames(entries []*nfsclient.EntryPlus) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}
