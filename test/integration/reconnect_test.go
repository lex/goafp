//go:build integration

package integration

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/lex/goafp/internal/afp"
	"github.com/lex/goafp/internal/dsi"
	"github.com/lex/goafp/internal/nfsfs"
	nfsclient "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// TestNFSReconnect proves the bridge survives losing its AFP connection:
// it mounts, kills afpd inside the container (dropping the connection while
// the port stays up as netatalk respawns it), and confirms operations
// recover once the server is back.
func TestNFSReconnect(t *testing.T) {
	container := os.Getenv("GOAFP_TEST_CONTAINER")
	if container == "" {
		t.Skip("GOAFP_TEST_CONTAINER not set; run via run.sh")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dial := func(dctx context.Context) (*dsi.Conn, *afp.Session, *afp.Volume, error) {
		info := fetchStatus(t)
		conn, err := dsi.Dial(dctx, cfg(t, "GOAFP_TEST_ADDR"))
		if err != nil {
			return nil, nil, nil, err
		}
		if err := conn.OpenSession(dctx); err != nil {
			conn.Close()
			return nil, nil, nil, err
		}
		sess := afp.NewSession(conn)
		if err := sess.Login(dctx, info, cfg(t, "GOAFP_TEST_USER"), cfg(t, "GOAFP_TEST_PASS")); err != nil {
			conn.Close()
			return nil, nil, nil, err
		}
		vol, err := sess.OpenVolume(dctx, cfg(t, "GOAFP_TEST_VOLUME"))
		if err != nil {
			conn.Close()
			return nil, nil, nil, err
		}
		return conn, sess, vol, nil
	}

	fsys, err := nfsfs.New(ctx, dial, 10*time.Second)
	if err != nil {
		t.Fatalf("nfsfs.New: %v", err)
	}
	defer fsys.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go nfsfs.Serve(l, fsys, 1024)
	defer l.Close()

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

	// Works before the disruption.
	if _, err := target.ReadDirPlus("/"); err != nil {
		t.Fatalf("ReadDirPlus before kill: %v", err)
	}

	// Drop the AFP connection by killing afpd; netatalk respawns it.
	if out, err := exec.Command("docker", "exec", container, "pkill", "-9", "afpd").CombinedOutput(); err != nil {
		t.Fatalf("failed to kill afpd: %v (%s)", err, out)
	}
	// Give netatalk a moment to notice and respawn the listener.
	time.Sleep(2 * time.Second)

	// Operations must recover once the server is back. The first attempts
	// may fail while afpd is restarting, so retry for a while.
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := target.ReadDirPlus("/"); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		t.Fatalf("did not recover after reconnect: %v", lastErr)
	}

	// And a fresh read of seeded content works post-reconnect.
	rf, err := target.Open("/hello.txt")
	if err != nil {
		t.Fatalf("open after reconnect: %v", err)
	}
	defer rf.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rf); err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}
	if buf.String() != "hello, world!\n" {
		t.Errorf("content after reconnect = %q", buf.String())
	}
}
