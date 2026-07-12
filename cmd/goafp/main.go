// Command goafp is a client for the Apple Filing Protocol.
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/lex/goafp/internal/afp"
	"github.com/lex/goafp/internal/dsi"
)

const usage = `usage: goafp <command> [arguments]

Commands:
  status  <host[:port]>                              query a server, no auth
  volumes <afp://[user[:pass]@]host>                 list volumes on a server
  ls      <afp://[user[:pass]@]host/volume[/path]>   list a directory
  cat     <afp://[user[:pass]@]host/volume/path>     stream a file to stdout
  get     <afp://[user[:pass]@]host/volume/path> [localfile]
                                                     download a file
  put     <localfile> <afp://[user[:pass]@]host/volume/path>
                                                     upload a file
  mkdir   <afp://[user[:pass]@]host/volume/path>     create a directory
  rm      <afp://[user[:pass]@]host/volume/path>     delete a file or empty dir
  mv      <afp://[user[:pass]@]host/volume/path> <newpath>
                                                     rename within the volume

With no user in the URL, goafp connects as a guest.
`

func main() {
	if len(os.Args) < 3 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "status":
		err = runStatus(args[0])
	case "volumes":
		err = runVolumes(args[0])
	case "ls":
		err = runLs(args[0])
	case "cat":
		err = runCat(args[0])
	case "get":
		err = runGet(args)
	case "put":
		err = runPut(args)
	case "mkdir":
		err = runMkdir(args[0])
	case "rm":
		err = runRm(args[0])
	case "mv":
		err = runMv(args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "goafp: %v\n", err)
		os.Exit(1)
	}
}

func runStatus(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := fetchStatus(ctx, addr)
	if err != nil {
		return err
	}

	fmt.Printf("Server name:  %s\n", info.ServerName)
	fmt.Printf("Machine type: %s\n", info.MachineType)
	fmt.Printf("AFP versions: %s\n", strings.Join(info.AFPVersions, ", "))
	fmt.Printf("UAMs:         %s\n", strings.Join(info.UAMs, ", "))
	fmt.Printf("Flags:        %#04x\n", info.Flags)
	return nil
}

// target is a parsed afp:// URL.
type target struct {
	host     string
	username string
	password string
	volume   string
	path     string
}

func parseTarget(raw string) (target, error) {
	if !strings.Contains(raw, "://") {
		raw = "afp://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return target{}, err
	}
	if u.Scheme != "afp" {
		return target{}, fmt.Errorf("unsupported scheme %q (want afp://)", u.Scheme)
	}
	t := target{host: u.Host}
	if u.User != nil {
		t.username = u.User.Username()
		t.password, _ = u.User.Password()
	}
	parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)
	if len(parts) > 0 {
		t.volume = parts[0]
	}
	if len(parts) > 1 {
		t.path = parts[1]
	}
	return t, nil
}

// connect dials, negotiates, and logs in; the caller must Close the
// returned connection.
//
// Server info (versions, UAMs) comes from DSIGetStatus, which some
// servers — netatalk among them — answer and then close the connection.
// So we fetch status on a throwaway connection and open a fresh one for
// the authenticated session.
func connect(ctx context.Context, t target) (*dsi.Conn, *afp.Session, error) {
	info, err := fetchStatus(ctx, t.host)
	if err != nil {
		return nil, nil, err
	}

	conn, err := dsi.Dial(ctx, t.host)
	if err != nil {
		return nil, nil, err
	}
	if err := conn.OpenSession(ctx); err != nil {
		conn.Close()
		return nil, nil, err
	}
	sess := afp.NewSession(conn)
	if err := sess.Login(ctx, info, t.username, t.password); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, sess, nil
}

func fetchStatus(ctx context.Context, host string) (*afp.ServerInfo, error) {
	conn, err := dsi.Dial(ctx, host)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	block, err := conn.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	return afp.ParseServerInfo(block)
}

func runVolumes(raw string) error {
	t, err := parseTarget(raw)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, sess, err := connect(ctx, t)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer sess.Logout(ctx)

	vols, err := sess.ListVolumes(ctx)
	if err != nil {
		return err
	}
	for _, v := range vols {
		note := ""
		if v.HasPassword {
			note = " (password protected)"
		}
		fmt.Printf("%s%s\n", v.Name, note)
	}
	return nil
}

func runLs(raw string) error {
	t, err := parseTarget(raw)
	if err != nil {
		return err
	}
	if t.volume == "" {
		return fmt.Errorf("URL must name a volume, e.g. afp://server/Volume")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, sess, err := connect(ctx, t)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, t.volume)
	if err != nil {
		return err
	}
	defer vol.Close(ctx)

	entries, err := vol.ReadDir(ctx, afp.RootDirID, t.path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		fmt.Println(formatEntry(e))
	}
	return nil
}

func runCat(raw string) error {
	return withFork(raw, func(ctx context.Context, f *afp.Fork) error {
		_, err := f.WriteToContext(ctx, os.Stdout)
		return err
	})
}

func runGet(args []string) error {
	raw := args[0]
	t, err := parseTarget(raw)
	if err != nil {
		return err
	}
	local := ""
	if len(args) > 1 {
		local = args[1]
	} else if t.path != "" {
		local = path.Base(t.path)
	}
	if local == "" || local == "." || local == "/" {
		return fmt.Errorf("cannot infer a local filename; pass one explicitly")
	}

	return withFork(raw, func(ctx context.Context, f *afp.Fork) error {
		out, err := os.Create(local)
		if err != nil {
			return err
		}
		defer out.Close()
		n, err := f.WriteToContext(ctx, out)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", local, n)
		return out.Close()
	})
}

func runPut(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: goafp put <localfile> <afp://.../volume/path>")
	}
	local, raw := args[0], args[1]
	t, err := parseTarget(raw)
	if err != nil {
		return err
	}
	if t.volume == "" || t.path == "" {
		return fmt.Errorf("destination URL must name a volume and a file path")
	}

	in, err := os.Open(local)
	if err != nil {
		return err
	}
	defer in.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	conn, sess, err := connect(ctx, t)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, t.volume)
	if err != nil {
		return err
	}
	defer vol.Close(ctx)

	// Overwrite any existing file, then open read/write and stream.
	if err := vol.CreateFile(ctx, afp.RootDirID, t.path, true); err != nil {
		return err
	}
	fork, err := vol.OpenForkRW(ctx, afp.RootDirID, t.path)
	if err != nil {
		return err
	}
	defer fork.Close(ctx)

	// The hard create above already replaced any existing file with an
	// empty one, so we can stream straight in.
	n, err := fork.ReadFromContext(ctx, in)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", t.path, n)
	return nil
}

func runMkdir(raw string) error {
	return withVolume(raw, func(ctx context.Context, vol *afp.Volume, t target) error {
		if t.path == "" {
			return fmt.Errorf("URL must name a directory path")
		}
		return vol.Mkdir(ctx, afp.RootDirID, t.path)
	})
}

func runRm(raw string) error {
	return withVolume(raw, func(ctx context.Context, vol *afp.Volume, t target) error {
		if t.path == "" {
			return fmt.Errorf("URL must name a path to delete")
		}
		return vol.Delete(ctx, afp.RootDirID, t.path)
	})
}

func runMv(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: goafp mv <afp://.../volume/path> <newpath>")
	}
	return withVolume(args[0], func(ctx context.Context, vol *afp.Volume, t target) error {
		if t.path == "" {
			return fmt.Errorf("source URL must name a path")
		}
		return vol.Rename(ctx, afp.RootDirID, t.path, strings.Trim(args[1], "/"))
	})
}

// withVolume resolves an afp:// URL to an open volume and runs fn.
func withVolume(raw string, fn func(context.Context, *afp.Volume, target) error) error {
	t, err := parseTarget(raw)
	if err != nil {
		return err
	}
	if t.volume == "" {
		return fmt.Errorf("URL must name a volume")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, sess, err := connect(ctx, t)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, t.volume)
	if err != nil {
		return err
	}
	defer vol.Close(ctx)

	return fn(ctx, vol, t)
}

// withFork resolves an afp:// URL down to an open data fork and runs fn.
func withFork(raw string, fn func(context.Context, *afp.Fork) error) error {
	t, err := parseTarget(raw)
	if err != nil {
		return err
	}
	if t.volume == "" || t.path == "" {
		return fmt.Errorf("URL must name a volume and a file path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	conn, sess, err := connect(ctx, t)
	if err != nil {
		return err
	}
	defer conn.Close()
	defer sess.Logout(ctx)

	vol, err := sess.OpenVolume(ctx, t.volume)
	if err != nil {
		return err
	}
	defer vol.Close(ctx)

	fork, err := vol.OpenFork(ctx, afp.RootDirID, t.path)
	if err != nil {
		return err
	}
	defer fork.Close(ctx)

	return fn(ctx, fork)
}

func formatEntry(e afp.DirEntry) string {
	kind := "-"
	size := fmt.Sprintf("%d", e.Size)
	if e.IsDir {
		kind = "d"
		size = fmt.Sprintf("%d items", e.Offspring)
	}
	perms := ""
	if e.HasUnix {
		perms = fmt.Sprintf(" %04o %5d:%-5d", e.UnixPrivs.Permissions&0o7777, e.UnixPrivs.UID, e.UnixPrivs.GID)
	}
	when := ""
	if !e.ModTime.IsZero() {
		when = e.ModTime.Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("%s%s %12s  %s  %s", kind, perms, size, when, e.Name)
}
