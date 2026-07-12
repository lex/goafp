package nfsfs

import (
	"io"
	"sync"

	"github.com/lex/goafp/internal/afp"
)

// File is a billy.File backed by an open AFP fork. A cursor supports the
// stream-style Read/Write/Seek that go-nfs uses for writes, while ReadAt
// serves positioned reads directly. The mutex prevents data races on the
// cursor when the NFS server touches one handle from multiple goroutines.
type File struct {
	fs       *FS
	fork     *afp.Fork
	name     string
	writable bool

	mu     sync.Mutex
	cursor int64
	size   int64
}

func (f *File) Name() string { return f.name }

func (f *File) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.fork.ReadAt(f.fs.ctx, p, f.cursor)
	f.cursor += int64(n)
	return n, err
}

func (f *File) ReadAt(p []byte, off int64) (int, error) {
	return f.fork.ReadAt(f.fs.ctx, p, off)
}

func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.fork.WriteAt(f.fs.ctx, p, f.cursor)
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
	if err := f.fork.Truncate(f.fs.ctx, size); err != nil {
		return mapErr(err)
	}
	f.mu.Lock()
	f.size = size
	f.mu.Unlock()
	return nil
}

func (f *File) Close() error {
	return f.fork.Close(f.fs.ctx)
}

// Lock and Unlock are no-ops: AFP byte-range locking is not wired through
// the bridge, and NFSv3 advisory locks are handled out of band.
func (f *File) Lock() error   { return nil }
func (f *File) Unlock() error { return nil }
