package afp

import (
	"context"
	"io"

	"golang.org/x/sync/errgroup"
)

// Open fork access mode bits.
const (
	forkRead  = 0x01
	forkWrite = 0x02
)

// Fork type bytes.
const (
	forkTypeData     = 0x00
	forkTypeResource = 0x80
)

// readChunkSize is the byte count of a single FPReadExt request. 128 KiB
// matches what modern macOS clients use; larger requests give the server
// less room to pipeline.
const readChunkSize = 128 * 1024

// maxConcurrentReads bounds how many FPReadExt requests a single ReadAt
// keeps in flight. The pipelined DSI core lets these overlap; this cap
// keeps memory and server load reasonable.
const maxConcurrentReads = 8

// Fork is an open file fork.
type Fork struct {
	v    *Volume
	ID   uint16
	Size uint64

	// Tunables, defaulted by OpenFork; overridable in tests.
	chunkSize   int
	maxParallel int
}

// OpenFork opens the data fork of the file at path (relative to dirID,
// normally RootDirID) for reading. The returned Fork must be Closed.
func (v *Volume) OpenFork(ctx context.Context, dirID uint32, path string) (*Fork, error) {
	var w builder
	w.u8(cmdOpenFork)
	w.u8(forkTypeData)
	w.u16(v.ID)
	w.u32(dirID)
	w.u16(kFPExtDataForkLenBit) // ask for the fork length
	w.u16(forkRead)
	w.path(path)

	payload, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return nil, err
	}
	if err := resultErr("open "+path, code); err != nil {
		return nil, err
	}

	// Reply: bitmap, fork ref num, then the requested parameters.
	r := &reader{b: payload}
	r.u16("open bitmap")
	fork := &Fork{
		v:           v,
		ID:          r.u16("fork id"),
		chunkSize:   readChunkSize,
		maxParallel: maxConcurrentReads,
	}
	fork.Size = r.u64("fork length")
	if r.err != nil {
		return nil, r.err
	}
	return fork, nil
}

// SetReadAhead tunes the read path: chunkSize is the size of each
// FPReadExt request and maxParallel is how many may be in flight at once.
// Non-positive values leave the current setting unchanged.
func (f *Fork) SetReadAhead(chunkSize, maxParallel int) {
	if chunkSize > 0 {
		f.chunkSize = chunkSize
	}
	if maxParallel > 0 {
		f.maxParallel = maxParallel
	}
}

// Close closes the fork (FPCloseFork).
func (f *Fork) Close(ctx context.Context) error {
	var w builder
	w.u8(cmdCloseFork)
	w.u8(0)
	w.u16(f.ID)
	_, code, err := f.v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("close fork", code)
}

// readChunk issues one FPReadExt. It returns the data and whether the
// read reached end of fork (a normal, non-error condition).
func (f *Fork) readChunk(ctx context.Context, offset int64, count int) (data []byte, eof bool, err error) {
	var w builder
	w.u8(cmdReadExt)
	w.u8(0)
	w.u16(f.ID)
	w.u64(uint64(offset))
	w.u64(uint64(count))

	payload, code, err := f.v.s.command(ctx, w.b)
	if err != nil {
		return nil, false, err
	}
	if code == ResEOF {
		return payload, true, nil
	}
	if err := resultErr("read", code); err != nil {
		return nil, false, err
	}
	return payload, false, nil
}

// ReadAt fills p with bytes starting at off, issuing the underlying
// FPReadExt requests concurrently. It returns the number of bytes read
// and io.EOF if the end of the fork was reached before filling p.
//
// This is where the pipelined DSI core pays off: a large read fans out
// into up to maxParallel overlapping requests rather than the strictly
// serial one-request-at-a-time model of the original afpfs-ng.
func (f *Fork) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	type chunk struct {
		start int // offset within p
		want  int // bytes requested
		data  []byte
		short bool // fewer bytes than requested, or EOF flagged
	}
	var chunks []*chunk
	for pos := 0; pos < len(p); pos += f.chunkSize {
		want := f.chunkSize
		if pos+want > len(p) {
			want = len(p) - pos
		}
		chunks = append(chunks, &chunk{start: pos, want: want})
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(f.maxParallel)
	for _, c := range chunks {
		c := c
		g.Go(func() error {
			data, eof, err := f.readChunk(ctx, off+int64(c.start), c.want)
			if err != nil {
				return err
			}
			c.data = data
			c.short = eof || len(data) < c.want
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return 0, err
	}

	// Reassemble in order. File data is contiguous, so a short chunk
	// means we hit EOF within it and nothing follows.
	n := 0
	for _, c := range chunks {
		n += copy(p[c.start:], c.data)
		if c.short {
			return n, io.EOF
		}
	}
	return n, nil
}

// WriteTo streams the entire fork to w, reading in windows that each
// pipeline up to maxParallel requests. It implements io.WriterTo.
func (f *Fork) WriteTo(w io.Writer) (int64, error) {
	return f.writeTo(context.Background(), w)
}

// WriteToContext is WriteTo with a caller-supplied context.
func (f *Fork) WriteToContext(ctx context.Context, w io.Writer) (int64, error) {
	return f.writeTo(ctx, w)
}

func (f *Fork) writeTo(ctx context.Context, w io.Writer) (int64, error) {
	buf := make([]byte, f.chunkSize*f.maxParallel)
	var total int64
	off := int64(0)
	for {
		n, err := f.ReadAt(ctx, buf, off)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			off += int64(n)
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
}
