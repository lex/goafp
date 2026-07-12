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

func TestWritePathRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess := login(t)
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, cfg(t, "GOAFP_TEST_VOLUME"))
	if err != nil {
		t.Fatalf("OpenVolume: %v", err)
	}
	defer vol.Close(ctx)

	// A subdirectory to keep the test's artifacts together.
	dir := "gotest-write"
	_ = vol.Delete(ctx, afp.RootDirID, dir) // best-effort cleanup from prior runs
	if err := vol.Mkdir(ctx, afp.RootDirID, dir); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		vol.Delete(cctx, afp.RootDirID, dir+"/renamed.bin")
		vol.Delete(cctx, afp.RootDirID, dir)
	})

	// Create and write a multi-chunk file.
	name := dir + "/upload.bin"
	if err := vol.CreateFile(ctx, afp.RootDirID, name, true); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	fork, err := vol.OpenForkRW(ctx, afp.RootDirID, name)
	if err != nil {
		t.Fatalf("OpenForkRW: %v", err)
	}
	fork.SetReadAhead(64*1024, 8)

	want := make([]byte, 500000)
	for i := range want {
		want[i] = byte(i % 251)
	}
	if _, err := fork.ReadFromContext(ctx, bytes.NewReader(want)); err != nil {
		t.Fatalf("ReadFrom (upload): %v", err)
	}
	if err := fork.Close(ctx); err != nil {
		t.Fatalf("close after write: %v", err)
	}

	// Read it back and compare.
	rfork, err := vol.OpenFork(ctx, afp.RootDirID, name)
	if err != nil {
		t.Fatalf("OpenFork (readback): %v", err)
	}
	if rfork.Size != uint64(len(want)) {
		t.Errorf("size after write = %d, want %d", rfork.Size, len(want))
	}
	var got bytes.Buffer
	if _, err := rfork.WriteToContext(ctx, &got); err != nil {
		t.Fatalf("WriteTo (readback): %v", err)
	}
	rfork.Close(ctx)
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("round-tripped content differs (%d vs %d bytes)", got.Len(), len(want))
	}

	// Truncate it to a smaller size and confirm.
	twork, err := vol.OpenForkRW(ctx, afp.RootDirID, name)
	if err != nil {
		t.Fatalf("OpenForkRW (truncate): %v", err)
	}
	if err := twork.Truncate(ctx, 100); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	twork.Close(ctx)
	if e, err := vol.Stat(ctx, afp.RootDirID, name); err != nil {
		t.Fatalf("stat after truncate: %v", err)
	} else if e.Size != 100 {
		t.Errorf("size after truncate = %d, want 100", e.Size)
	}

	// Rename it.
	if err := vol.Rename(ctx, afp.RootDirID, name, dir+"/renamed.bin"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := vol.Stat(ctx, afp.RootDirID, dir+"/renamed.bin"); err != nil {
		t.Errorf("stat after rename: %v", err)
	}

	// Delete it.
	if err := vol.Delete(ctx, afp.RootDirID, dir+"/renamed.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := vol.Stat(ctx, afp.RootDirID, dir+"/renamed.bin"); err == nil {
		t.Error("file still present after delete")
	}
}

func names(entries []afp.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}
