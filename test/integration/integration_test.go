//go:build integration

// Package integration tests goafp against a real AFP server (normally
// netatalk in Docker; see run.sh). Configuration comes from the
// environment:
//
//	GOAFP_TEST_ADDR    host:port of the AFP server
//	GOAFP_TEST_USER    username
//	GOAFP_TEST_PASS    password
//	GOAFP_TEST_VOLUME  volume name to open
package integration

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/lex/goafp/internal/afp"
	"github.com/lex/goafp/internal/dsi"
)

func cfg(t *testing.T, key string) string {
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set; run via test/integration/run.sh", key)
	}
	return v
}

// fetchStatus fetches server info on its own connection. Some servers
// (netatalk) close the connection right after answering DSIGetStatus, so
// the authenticated session must use a fresh connection.
func fetchStatus(t *testing.T) *afp.ServerInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := dsi.Dial(ctx, cfg(t, "GOAFP_TEST_ADDR"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	block, err := conn.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	info, err := afp.ParseServerInfo(block)
	if err != nil {
		t.Fatalf("ParseServerInfo: %v", err)
	}
	return info
}

func login(t *testing.T) *afp.Session {
	t.Helper()
	info := fetchStatus(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := dsi.Dial(ctx, cfg(t, "GOAFP_TEST_ADDR"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	if err := conn.OpenSession(ctx); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	sess := afp.NewSession(conn)
	err = sess.Login(ctx, info, cfg(t, "GOAFP_TEST_USER"), cfg(t, "GOAFP_TEST_PASS"))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return sess
}

func TestStatus(t *testing.T) {
	info := fetchStatus(t)
	if len(info.AFPVersions) == 0 || len(info.UAMs) == 0 {
		t.Errorf("thin server info: %+v", info)
	}
	t.Logf("server %q machine %q versions %v uams %v",
		info.ServerName, info.MachineType, info.AFPVersions, info.UAMs)
}

func TestLoginAndListVolumes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess := login(t)
	defer sess.Logout(ctx)

	vols, err := sess.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	want := cfg(t, "GOAFP_TEST_VOLUME")
	found := false
	for _, v := range vols {
		t.Logf("volume: %+v", v)
		if v.Name == want {
			found = true
		}
	}
	if !found {
		t.Errorf("volume %q not in %+v", want, vols)
	}
}

func TestReadDirAndStat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := login(t)
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, cfg(t, "GOAFP_TEST_VOLUME"))
	if err != nil {
		t.Fatalf("OpenVolume: %v", err)
	}
	defer vol.Close(ctx)

	entries, err := vol.ReadDir(ctx, afp.RootDirID, "")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	byName := map[string]afp.DirEntry{}
	for _, e := range entries {
		t.Logf("entry: %+v", e)
		byName[e.Name] = e
	}

	// Seeded by run.sh.
	f, ok := byName["hello.txt"]
	if !ok {
		t.Fatalf("hello.txt missing from %v", names(entries))
	}
	if f.IsDir || f.Size != 14 {
		t.Errorf("hello.txt = %+v, want 14-byte file", f)
	}
	d, ok := byName["subdir"]
	if !ok || !d.IsDir {
		t.Errorf("subdir missing or not a dir: %+v", d)
	}
	if _, ok := byName["smörgåsbord.txt"]; !ok {
		t.Errorf("unicode name missing (decomposed->precomposed conversion?): %v", names(entries))
	}

	// Stat a file inside the subdirectory by path.
	e, err := vol.Stat(ctx, afp.RootDirID, "subdir/nested.txt")
	if err != nil {
		t.Fatalf("Stat subdir/nested.txt: %v", err)
	}
	if e.IsDir || e.Size != 7 {
		t.Errorf("nested.txt = %+v, want 7-byte file", e)
	}

	// Enumerating the subdirectory by path.
	sub, err := vol.ReadDir(ctx, afp.RootDirID, "subdir")
	if err != nil {
		t.Fatalf("ReadDir subdir: %v", err)
	}
	if len(sub) != 1 || sub[0].Name != "nested.txt" {
		t.Errorf("subdir contents = %v", names(sub))
	}
}

func TestReadFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := login(t)
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, cfg(t, "GOAFP_TEST_VOLUME"))
	if err != nil {
		t.Fatalf("OpenVolume: %v", err)
	}
	defer vol.Close(ctx)

	fork, err := vol.OpenFork(ctx, afp.RootDirID, "hello.txt")
	if err != nil {
		t.Fatalf("OpenFork: %v", err)
	}
	defer fork.Close(ctx)

	if fork.Size != 14 {
		t.Errorf("fork size = %d, want 14", fork.Size)
	}

	var buf bytes.Buffer
	n, err := fork.WriteToContext(ctx, &buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if want := "hello, world!\n"; buf.String() != want || n != int64(len(want)) {
		t.Errorf("file contents = %q (%d bytes), want %q", buf.String(), n, want)
	}
}

// TestReadLargeFilePipelined seeds a multi-megabyte file, reads it back,
// and checks it comes through intact across many pipelined chunks.
func TestReadLargeFilePipelined(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess := login(t)
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, cfg(t, "GOAFP_TEST_VOLUME"))
	if err != nil {
		t.Fatalf("OpenVolume: %v", err)
	}
	defer vol.Close(ctx)

	fork, err := vol.OpenFork(ctx, afp.RootDirID, "big.bin")
	if err != nil {
		t.Skipf("big.bin not present (seeded by run.sh): %v", err)
	}
	defer fork.Close(ctx)

	// Small chunks so a few MB exercises many concurrent requests.
	fork.SetReadAhead(64*1024, 8)

	var buf bytes.Buffer
	n, err := fork.WriteToContext(ctx, &buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if uint64(n) != fork.Size {
		t.Errorf("read %d bytes, fork size %d", n, fork.Size)
	}
	// run.sh fills big.bin with a repeating byte pattern.
	for i, b := range buf.Bytes() {
		if b != byte(i%251) {
			t.Fatalf("byte %d = %d, want %d (pipelined reassembly bug?)", i, b, i%251)
		}
	}
}

func names(entries []afp.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}
