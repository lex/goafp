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
	"errors"
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

func TestSetAttrAndStatFS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := login(t)
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, cfg(t, "GOAFP_TEST_VOLUME"))
	if err != nil {
		t.Fatalf("OpenVolume: %v", err)
	}
	defer vol.Close(ctx)

	// StatFS should report a real, non-empty volume.
	fsinfo, err := vol.StatFS(ctx)
	if err != nil {
		t.Fatalf("StatFS: %v", err)
	}
	if fsinfo.TotalBytes == 0 || fsinfo.BlockSize == 0 {
		t.Errorf("StatFS returned empty info: %+v", fsinfo)
	}
	t.Logf("statfs: total=%d free=%d bsize=%d", fsinfo.TotalBytes, fsinfo.FreeBytes, fsinfo.BlockSize)

	if !vol.SupportsUnixPrivs() {
		t.Skip("volume has no unix privs; skipping chmod/chtimes checks")
	}

	name := "gotest-attr.txt"
	if err := vol.CreateFile(ctx, afp.RootDirID, name, true); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		vol.Delete(cctx, afp.RootDirID, name)
	})

	before, err := vol.Stat(ctx, afp.RootDirID, name)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// chmod to 0600, preserving the file-type bits.
	const sIFMT = 0o170000
	newMode := uint32(0o600) | (before.UnixPrivs.Permissions & sIFMT)
	if err := vol.SetUnixPrivs(ctx, afp.RootDirID, name, before.UnixPrivs.UID, before.UnixPrivs.GID, newMode); err != nil {
		t.Fatalf("SetUnixPrivs: %v", err)
	}
	after, err := vol.Stat(ctx, afp.RootDirID, name)
	if err != nil {
		t.Fatalf("stat after chmod: %v", err)
	}
	if got := after.UnixPrivs.Permissions & 0o777; got != 0o600 {
		t.Errorf("permissions after chmod = %o, want 600", got)
	}

	// set modification time and confirm it round-trips (within a second).
	want := time.Date(2021, 6, 15, 12, 30, 0, 0, time.UTC)
	if err := vol.SetModTime(ctx, afp.RootDirID, name, want); err != nil {
		t.Fatalf("SetModTime: %v", err)
	}
	got, err := vol.Stat(ctx, afp.RootDirID, name)
	if err != nil {
		t.Fatalf("stat after utime: %v", err)
	}
	if diff := got.ModTime.Sub(want); diff > time.Second || diff < -time.Second {
		t.Errorf("mod time after set = %v, want ~%v", got.ModTime, want)
	}
}

func TestSymlink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := login(t)
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, cfg(t, "GOAFP_TEST_VOLUME"))
	if err != nil {
		t.Fatalf("OpenVolume: %v", err)
	}
	defer vol.Close(ctx)

	link := "gotest-link"
	target := "subdir/nested.txt"
	_ = vol.Delete(ctx, afp.RootDirID, link)
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		vol.Delete(cctx, afp.RootDirID, link)
	})

	if err := vol.Symlink(ctx, afp.RootDirID, link, target); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Stat should now report it as a symlink.
	e, err := vol.Stat(ctx, afp.RootDirID, link)
	if err != nil {
		t.Fatalf("Stat link: %v", err)
	}
	if !e.IsSymlink {
		t.Errorf("created object is not reported as a symlink: %+v", e)
	}

	// And the target must read back.
	got, err := vol.ReadLink(ctx, afp.RootDirID, link)
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if got != target {
		t.Errorf("readlink = %q, want %q", got, target)
	}

	// It should also appear as a symlink in an enumeration.
	entries, err := vol.ReadDir(ctx, afp.RootDirID, "")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	for _, ent := range entries {
		if ent.Name == link {
			found = true
			if !ent.IsSymlink {
				t.Errorf("enumerated %q is not a symlink", link)
			}
		}
	}
	if !found {
		t.Errorf("%q missing from enumeration", link)
	}
}

// TestSRPLive exercises the SRP UAM against netatalk. The server advertises
// both DHX2 and SRP, so we force SRP by presenting only it to Login. run.sh
// provisions the verifier; if that failed the login returns not-authenticated
// and we skip.
func TestSRPLive(t *testing.T) {
	info := fetchStatus(t)
	hasSRP := false
	for _, u := range info.UAMs {
		if u == "SRP" {
			hasSRP = true
		}
	}
	if !hasSRP {
		t.Skip("server does not offer SRP")
	}

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

	srpOnly := &afp.ServerInfo{AFPVersions: info.AFPVersions, UAMs: []string{"SRP"}}
	sess := afp.NewSession(conn)
	err = sess.Login(ctx, srpOnly, cfg(t, "GOAFP_TEST_USER"), cfg(t, "GOAFP_TEST_PASS"))
	if err != nil {
		var ae *afp.Error
		if errors.As(err, &ae) && ae.Code == afp.ResUserNotAuth {
			t.Skip("no SRP verifier provisioned; skipping (see run.sh)")
		}
		t.Fatalf("SRP login: %v", err)
	}
	defer sess.Logout(ctx)

	// Prove the authenticated session works.
	vols, err := sess.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes after SRP login: %v", err)
	}
	want := cfg(t, "GOAFP_TEST_VOLUME")
	found := false
	for _, v := range vols {
		if v.Name == want {
			found = true
		}
	}
	if !found {
		t.Errorf("volume %q not found after SRP login: %+v", want, vols)
	}
}

func names(entries []afp.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}
