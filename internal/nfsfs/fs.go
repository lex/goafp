// Package nfsfs adapts an AFP volume to the go-billy filesystem interface
// so it can be served over NFS by github.com/willscott/go-nfs. This is how
// goafp presents a remote AFP share as a locally mountable filesystem on
// both Linux and macOS without any kernel extension: the OS mounts a
// localhost NFSv3 server that this package backs with AFP calls.
//
// The filesystem transparently reconnects when the AFP link drops (server
// sleep, network blip, idle timeout) and bounds every operation with a
// timeout, so a mount survives transient failures instead of wedging.
package nfsfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/lex/goafp/internal/afp"
)

// DefaultOpTimeout bounds a single filesystem operation before it is
// treated as a failure (and a reconnect is attempted).
const DefaultOpTimeout = 60 * time.Second

// FS is a billy.Filesystem backed by an AFP volume.
type FS struct {
	cm   *connManager
	base string // chroot prefix relative to the volume root
}

// New establishes the initial connection via dial and returns a
// filesystem. base bounds the mount's lifetime; opTimeout (0 means
// DefaultOpTimeout) bounds each operation.
func New(base context.Context, dial Dialer, opTimeout time.Duration) (*FS, error) {
	if opTimeout <= 0 {
		opTimeout = DefaultOpTimeout
	}
	cm, err := newConnManager(base, dial, opTimeout)
	if err != nil {
		return nil, err
	}
	return &FS{cm: cm}, nil
}

// Close tears down the underlying connection.
func (f *FS) Close() { f.cm.close() }

// resolve turns a billy path into an AFP path relative to the volume root
// ("" means the root itself).
func (f *FS) resolve(name string) string {
	p := path.Join("/", f.base, name)
	return strings.TrimPrefix(p, "/")
}

// mapErr converts AFP errors into the os sentinels go-nfs recognizes.
func mapErr(err error) error {
	var ae *afp.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case afp.ResObjectNotFound, afp.ResDirNotFound, afp.ResItemNotFound:
			return os.ErrNotExist
		case afp.ResAccessDenied, afp.ResUserNotAuth:
			return os.ErrPermission
		case afp.ResObjectExists:
			return os.ErrExist
		}
	}
	return err
}

func (f *FS) supportsUnixPrivs() bool {
	// The volume flag is stable across reconnects; sample the current one.
	su := false
	f.cm.do(func(_ context.Context, vol *afp.Volume) error {
		su = vol.SupportsUnixPrivs()
		return nil
	})
	return su
}

// --- billy.Basic ---

func (f *FS) Create(filename string) (billy.File, error) {
	return f.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
}

func (f *FS) Open(filename string) (billy.File, error) {
	return f.OpenFile(filename, os.O_RDONLY, 0)
}

func (f *FS) OpenFile(filename string, flag int, _ os.FileMode) (billy.File, error) {
	p := f.resolve(filename)
	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0

	fp := &File{cm: f.cm, name: filename, path: p, writable: writable}
	err := f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		if flag&os.O_CREATE != 0 {
			if err := vol.CreateFile(ctx, afp.RootDirID, p, false); err != nil {
				if e := mapErr(err); errors.Is(e, os.ErrExist) {
					if flag&os.O_EXCL != 0 {
						return &os.PathError{Op: "open", Path: filename, Err: os.ErrExist}
					}
				} else {
					return &os.PathError{Op: "open", Path: filename, Err: e}
				}
			}
		}
		fork, err := fp.open(ctx, vol)
		if err != nil {
			return &os.PathError{Op: "open", Path: filename, Err: mapErr(err)}
		}
		fp.size = int64(fork.Size)
		if flag&os.O_TRUNC != 0 && writable {
			if err := fork.Truncate(ctx, 0); err != nil {
				return &os.PathError{Op: "truncate", Path: filename, Err: mapErr(err)}
			}
			fp.size = 0
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if flag&os.O_APPEND != 0 {
		fp.cursor = fp.size
	}
	return fp, nil
}

func (f *FS) Stat(filename string) (os.FileInfo, error) {
	p := f.resolve(filename)
	if p == "" {
		return &fileInfo{name: "/", mode: os.ModeDir | 0755, isDir: true}, nil
	}
	var e afp.DirEntry
	err := f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		var er error
		e, er = vol.Stat(ctx, afp.RootDirID, p)
		return er
	})
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: filename, Err: mapErr(err)}
	}
	return entryInfo(path.Base(p), e), nil
}

func (f *FS) Lstat(filename string) (os.FileInfo, error) {
	// Stat already reports symlinks as symlinks (it reads Finder info),
	// which is what NFS wants: the server describes the link and the
	// client resolves it via Readlink.
	return f.Stat(filename)
}

func (f *FS) Rename(oldpath, newpath string) error {
	err := f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		return vol.Rename(ctx, afp.RootDirID, f.resolve(oldpath), f.resolve(newpath))
	})
	if err != nil {
		return &os.PathError{Op: "rename", Path: oldpath, Err: mapErr(err)}
	}
	return nil
}

func (f *FS) Remove(filename string) error {
	err := f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		return vol.Delete(ctx, afp.RootDirID, f.resolve(filename))
	})
	if err != nil {
		return &os.PathError{Op: "remove", Path: filename, Err: mapErr(err)}
	}
	return nil
}

func (f *FS) Join(elem ...string) string {
	return path.Join(elem...)
}

// --- billy.Dir ---

func (f *FS) ReadDir(dirname string) ([]os.FileInfo, error) {
	var entries []afp.DirEntry
	err := f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		var er error
		entries, er = vol.ReadDir(ctx, afp.RootDirID, f.resolve(dirname))
		return er
	})
	if err != nil {
		return nil, &os.PathError{Op: "readdir", Path: dirname, Err: mapErr(err)}
	}
	out := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryInfo(e.Name, e))
	}
	return out, nil
}

func (f *FS) MkdirAll(filename string, _ os.FileMode) error {
	p := f.resolve(filename)
	if p == "" {
		return nil
	}
	// AFP has no recursive mkdir; create each missing component.
	parts := strings.Split(p, "/")
	for i := range parts {
		sub := strings.Join(parts[:i+1], "/")
		err := f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
			return vol.Mkdir(ctx, afp.RootDirID, sub)
		})
		if err != nil {
			if e := mapErr(err); errors.Is(e, os.ErrExist) {
				continue
			}
			return &os.PathError{Op: "mkdir", Path: sub, Err: mapErr(err)}
		}
	}
	return nil
}

// --- billy.TempFile ---

func (f *FS) TempFile(dir, prefix string) (billy.File, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	name := f.Join(dir, prefix+hex.EncodeToString(b[:]))
	return f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
}

// --- billy.Symlink ---

func (f *FS) Symlink(target, link string) error {
	err := f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		return vol.Symlink(ctx, afp.RootDirID, f.resolve(link), target)
	})
	if err != nil {
		return &os.PathError{Op: "symlink", Path: link, Err: mapErr(err)}
	}
	return nil
}

func (f *FS) Readlink(link string) (string, error) {
	var t string
	err := f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		var er error
		t, er = vol.ReadLink(ctx, afp.RootDirID, f.resolve(link))
		return er
	})
	if err != nil {
		return "", &os.PathError{Op: "readlink", Path: link, Err: mapErr(err)}
	}
	return t, nil
}

// --- billy.Chroot ---

func (f *FS) Chroot(p string) (billy.Filesystem, error) {
	return &FS{cm: f.cm, base: f.resolve(p)}, nil
}

func (f *FS) Root() string {
	if f.base == "" {
		return "/"
	}
	return "/" + f.base
}

// --- billy.Change ---
//
// chmod/chown map onto AFP unix privileges (only on volumes that carry
// them; otherwise they succeed as no-ops so NFS clients aren't blocked).
// chtimes maps onto the modification date. Access-time changes have no AFP
// equivalent and are ignored.

const sIFMT = 0o170000 // file-type mask within a unix mode

func (f *FS) Chmod(name string, mode os.FileMode) error {
	if !f.supportsUnixPrivs() {
		return nil
	}
	p := f.resolve(name)
	return f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		e, err := vol.Stat(ctx, afp.RootDirID, p)
		if err != nil {
			return mapErr(err)
		}
		perm := (uint32(mode) & 0o7777) | (e.UnixPrivs.Permissions & sIFMT)
		return mapErr(vol.SetUnixPrivs(ctx, afp.RootDirID, p, e.UnixPrivs.UID, e.UnixPrivs.GID, perm))
	})
}

func (f *FS) Lchown(name string, uid, gid int) error { return f.chown(name, uid, gid) }
func (f *FS) Chown(name string, uid, gid int) error  { return f.chown(name, uid, gid) }

func (f *FS) chown(name string, uid, gid int) error {
	if !f.supportsUnixPrivs() {
		return nil
	}
	p := f.resolve(name)
	return f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		e, err := vol.Stat(ctx, afp.RootDirID, p)
		if err != nil {
			return mapErr(err)
		}
		newUID, newGID := e.UnixPrivs.UID, e.UnixPrivs.GID
		if uid >= 0 {
			newUID = uint32(uid)
		}
		if gid >= 0 {
			newGID = uint32(gid)
		}
		return mapErr(vol.SetUnixPrivs(ctx, afp.RootDirID, p, newUID, newGID, e.UnixPrivs.Permissions))
	})
}

func (f *FS) Chtimes(name string, atime, mtime time.Time) error {
	p := f.resolve(name)
	return f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		return mapErr(vol.SetModTime(ctx, afp.RootDirID, p, mtime))
	})
}

// fileInfo implements os.FileInfo for an AFP entry.
type fileInfo struct {
	name  string
	size  int64
	mode  os.FileMode
	mod   time.Time
	isDir bool
}

func entryInfo(name string, e afp.DirEntry) *fileInfo {
	mode := os.FileMode(e.UnixPrivs.Permissions & 0o777)
	if mode == 0 {
		if e.IsDir {
			mode = 0o755
		} else {
			mode = 0o644
		}
	}
	switch {
	case e.IsSymlink:
		mode |= os.ModeSymlink
	case e.IsDir:
		mode |= os.ModeDir
	}
	return &fileInfo{
		name:  name,
		size:  int64(e.Size),
		mode:  mode,
		mod:   e.ModTime,
		isDir: e.IsDir,
	}
}

func (fi *fileInfo) Name() string       { return fi.name }
func (fi *fileInfo) Size() int64        { return fi.size }
func (fi *fileInfo) Mode() os.FileMode  { return fi.mode }
func (fi *fileInfo) ModTime() time.Time { return fi.mod }
func (fi *fileInfo) IsDir() bool        { return fi.isDir }
func (fi *fileInfo) Sys() interface{}   { return nil }
