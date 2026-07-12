// Package nfsfs adapts an AFP volume to the go-billy filesystem interface
// so it can be served over NFS by github.com/willscott/go-nfs. This is how
// goafp presents a remote AFP share as a locally mountable filesystem on
// both Linux and macOS without any kernel extension: the OS mounts a
// localhost NFSv3 server that this package backs with AFP calls.
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

// FS is a billy.Filesystem backed by an AFP volume. A single FS (and its
// underlying pipelined DSI connection) is shared across all NFS requests;
// AFP operations are safe to issue concurrently.
type FS struct {
	vol  *afp.Volume
	ctx  context.Context
	base string // chroot prefix relative to the volume root
}

// New returns an FS serving vol. ctx bounds the lifetime of AFP operations.
func New(ctx context.Context, vol *afp.Volume) *FS {
	return &FS{vol: vol, ctx: ctx}
}

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

	if flag&os.O_CREATE != 0 {
		// O_EXCL only matters alongside O_CREATE.
		excl := flag&os.O_EXCL != 0
		err := f.vol.CreateFile(f.ctx, afp.RootDirID, p, false)
		if err != nil {
			if e := mapErr(err); errors.Is(e, os.ErrExist) {
				if excl {
					return nil, &os.PathError{Op: "open", Path: filename, Err: os.ErrExist}
				}
			} else {
				return nil, &os.PathError{Op: "open", Path: filename, Err: e}
			}
		}
	}

	var (
		fork *afp.Fork
		err  error
	)
	if writable {
		fork, err = f.vol.OpenForkRW(f.ctx, afp.RootDirID, p)
	} else {
		fork, err = f.vol.OpenFork(f.ctx, afp.RootDirID, p)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: filename, Err: mapErr(err)}
	}

	fp := &File{fs: f, fork: fork, name: filename, writable: writable}
	if flag&os.O_TRUNC != 0 && writable {
		if err := fork.Truncate(f.ctx, 0); err != nil {
			fork.Close(f.ctx)
			return nil, &os.PathError{Op: "truncate", Path: filename, Err: mapErr(err)}
		}
		fp.size = 0
	} else {
		fp.size = int64(fork.Size)
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
	e, err := f.vol.Stat(f.ctx, afp.RootDirID, p)
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: filename, Err: mapErr(err)}
	}
	return entryInfo(path.Base(p), e), nil
}

func (f *FS) Lstat(filename string) (os.FileInfo, error) {
	// AFP symlinks are not yet distinguished; treat as a normal stat.
	return f.Stat(filename)
}

func (f *FS) Rename(oldpath, newpath string) error {
	if err := f.vol.Rename(f.ctx, afp.RootDirID, f.resolve(oldpath), f.resolve(newpath)); err != nil {
		return &os.PathError{Op: "rename", Path: oldpath, Err: mapErr(err)}
	}
	return nil
}

func (f *FS) Remove(filename string) error {
	if err := f.vol.Delete(f.ctx, afp.RootDirID, f.resolve(filename)); err != nil {
		return &os.PathError{Op: "remove", Path: filename, Err: mapErr(err)}
	}
	return nil
}

func (f *FS) Join(elem ...string) string {
	return path.Join(elem...)
}

// --- billy.Dir ---

func (f *FS) ReadDir(dirname string) ([]os.FileInfo, error) {
	entries, err := f.vol.ReadDir(f.ctx, afp.RootDirID, f.resolve(dirname))
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
	// AFP has no recursive mkdir; create each missing component.
	p := f.resolve(filename)
	if p == "" {
		return nil
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		sub := strings.Join(parts[:i+1], "/")
		err := f.vol.Mkdir(f.ctx, afp.RootDirID, sub)
		if err != nil {
			if e := mapErr(err); errors.Is(e, os.ErrExist) {
				continue
			} else {
				return &os.PathError{Op: "mkdir", Path: sub, Err: mapErr(err)}
			}
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

// --- billy.Symlink (unsupported for now) ---

var errNoSymlink = errors.New("afp: symlinks are not supported over the NFS bridge")

func (f *FS) Symlink(target, link string) error { return errNoSymlink }
func (f *FS) Readlink(link string) (string, error) {
	return "", &os.PathError{Op: "readlink", Path: link, Err: errNoSymlink}
}

// --- billy.Chroot ---

func (f *FS) Chroot(p string) (billy.Filesystem, error) {
	return &FS{vol: f.vol, ctx: f.ctx, base: f.resolve(p)}, nil
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
	if !f.vol.SupportsUnixPrivs() {
		return nil
	}
	p := f.resolve(name)
	e, err := f.vol.Stat(f.ctx, afp.RootDirID, p)
	if err != nil {
		return mapErr(err)
	}
	// Preserve the file-type bits; replace the permission bits.
	perm := (uint32(mode) & 0o7777) | (e.UnixPrivs.Permissions & sIFMT)
	return mapErr(f.vol.SetUnixPrivs(f.ctx, afp.RootDirID, p, e.UnixPrivs.UID, e.UnixPrivs.GID, perm))
}

func (f *FS) Lchown(name string, uid, gid int) error { return f.chown(name, uid, gid) }
func (f *FS) Chown(name string, uid, gid int) error  { return f.chown(name, uid, gid) }

func (f *FS) chown(name string, uid, gid int) error {
	if !f.vol.SupportsUnixPrivs() {
		return nil
	}
	p := f.resolve(name)
	e, err := f.vol.Stat(f.ctx, afp.RootDirID, p)
	if err != nil {
		return mapErr(err)
	}
	// A negative id means "leave unchanged" (POSIX chown convention).
	newUID, newGID := e.UnixPrivs.UID, e.UnixPrivs.GID
	if uid >= 0 {
		newUID = uint32(uid)
	}
	if gid >= 0 {
		newGID = uint32(gid)
	}
	return mapErr(f.vol.SetUnixPrivs(f.ctx, afp.RootDirID, p, newUID, newGID, e.UnixPrivs.Permissions))
}

func (f *FS) Chtimes(name string, atime, mtime time.Time) error {
	return mapErr(f.vol.SetModTime(f.ctx, afp.RootDirID, f.resolve(name), mtime))
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
	if e.IsDir {
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
