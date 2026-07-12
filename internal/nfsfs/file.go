package nfsfs

import (
	"context"
	"io"
	"sync"

	"github.com/lex/goafp/internal/afp"
)

// File is a billy.File backed by an open AFP fork. Because a fork ID is
// only valid within one connection generation, the File remembers its path
// and re-opens the fork transparently after a reconnect (detected by the
// volume pointer changing). A cursor supports the stream-style
// Read/Write/Seek that go-nfs uses for writes.
type File struct {
	cm       *connManager
	name     string // billy path (for Name / errors)
	path     string // resolved AFP path
	writable bool

	mu      sync.Mutex
	fork    *afp.Fork
	forkVol *afp.Volume // the volume generation the fork belongs to
	cursor  int64
	size    int64
}

func (f *File) Name() string { return f.name }

// open (re)opens the fork against vol. Callers hold f.mu (or are within
// OpenFile before the File is shared).
func (f *File) open(ctx context.Context, vol *afp.Volume) (*afp.Fork, error) {
	if f.fork != nil && f.forkVol == vol {
		return f.fork, nil
	}
	var (
		fork *afp.Fork
		err  error
	)
	if f.writable {
		fork, err = vol.OpenForkRW(ctx, afp.RootDirID, f.path)
	} else {
		fork, err = vol.OpenFork(ctx, afp.RootDirID, f.path)
	}
	if err != nil {
		return nil, err
	}
	f.fork = fork
	f.forkVol = vol
	return fork, nil
}

// withFork runs fn against a valid fork, re-opening it if a reconnect has
// invalidated the previous one. Callers hold f.mu.
func (f *File) withFork(fn func(ctx context.Context, fork *afp.Fork) error) error {
	return f.cm.do(func(ctx context.Context, vol *afp.Volume) error {
		fork, err := f.open(ctx, vol)
		if err != nil {
			return err
		}
		return fn(ctx, fork)
	})
}

func (f *File) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	err := f.withFork(func(ctx context.Context, fork *afp.Fork) error {
		var e error
		n, e = fork.ReadAt(ctx, p, f.cursor)
		return e
	})
	f.cursor += int64(n)
	return n, err
}

func (f *File) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	err := f.withFork(func(ctx context.Context, fork *afp.Fork) error {
		var e error
		n, e = fork.ReadAt(ctx, p, off)
		return e
	})
	return n, err
}

func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	err := f.withFork(func(ctx context.Context, fork *afp.Fork) error {
		var e error
		n, e = fork.WriteAt(ctx, p, f.cursor)
		return e
	})
	f.cursor += int64(n)
	if f.cursor > f.size {
		f.size = f.cursor
	}
	return n, err
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch whence {
	case io.SeekStart:
		f.cursor = offset
	case io.SeekCurrent:
		f.cursor += offset
	case io.SeekEnd:
		f.cursor = f.size + offset
	}
	return f.cursor, nil
}

func (f *File) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.withFork(func(ctx context.Context, fork *afp.Fork) error {
		return fork.Truncate(ctx, size)
	})
	if err != nil {
		return mapErr(err)
	}
	f.size = size
	return nil
}

func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fork == nil {
		return nil
	}
	fork, vol := f.fork, f.forkVol
	f.fork, f.forkVol = nil, nil
	// Only close if the fork still belongs to the current generation; a
	// fork from a superseded connection was already dropped with its
	// socket, and its ID would mean something else now.
	return f.cm.do(func(ctx context.Context, cur *afp.Volume) error {
		if cur != vol {
			return nil
		}
		return fork.Close(ctx)
	})
}

// Lock and Unlock are no-ops: AFP byte-range locking is not wired through
// the bridge, and NFSv3 advisory locks are handled out of band.
func (f *File) Lock() error   { return nil }
func (f *File) Unlock() error { return nil }
