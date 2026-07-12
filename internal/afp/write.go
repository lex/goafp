package afp

import (
	"context"
	"io"

	"golang.org/x/sync/errgroup"
)

// writeChunkHeaderLen is the size of the FPWriteExt command block that
// precedes the bulk data: command, flag, fork id, offset, reqcount.
const writeChunkHeaderLen = 1 + 1 + 2 + 8 + 8

// writeChunk issues one FPWriteExt of data at offset. AFP writes are
// all-or-nothing, so on success the whole slice was written.
func (f *Fork) writeChunk(ctx context.Context, offset int64, data []byte) (int, error) {
	var w builder
	w.u8(cmdWriteExt)
	w.u8(0) // flag 0: offset is from the start of the fork
	w.u16(f.ID)
	w.u64(uint64(offset))
	w.u64(uint64(len(data)))
	dataOffset := len(w.b) // == writeChunkHeaderLen
	w.bytes(data)

	_, code, err := f.v.s.commandWrite(ctx, uint32(dataOffset), w.b)
	if err != nil {
		return 0, err
	}
	if err := resultErr("write", code); err != nil {
		return 0, err
	}
	return len(data), nil
}

// writeChunkSize returns the effective chunk size for writes: the
// configured size, capped so the whole DSI request stays within the
// server's advertised request quantum.
func (f *Fork) writeChunkSize() int {
	chunk := f.chunkSize
	if q := int(f.v.s.serverQuantum()); q > 0 {
		if max := q - writeChunkHeaderLen; max > 0 && chunk > max {
			chunk = max
		}
	}
	if chunk <= 0 {
		chunk = 32 * 1024
	}
	return chunk
}

// WriteAt writes p at offset off, splitting it into chunks that are sent
// as concurrent FPWriteExt requests (bounded by maxParallel). It returns
// the number of bytes written.
//
// Coalescing small writes up to the server quantum and pipelining the
// chunks is the direct fix for afpfs-ng's worst bottleneck: one network
// round trip per 4 KB.
func (f *Fork) WriteAt(ctx context.Context, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	chunk := f.writeChunkSize()

	type piece struct{ start, n int }
	var pieces []piece
	for pos := 0; pos < len(p); pos += chunk {
		end := pos + chunk
		if end > len(p) {
			end = len(p)
		}
		pieces = append(pieces, piece{pos, end - pos})
	}

	written := make([]int, len(pieces))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(f.maxParallel)
	for i, pc := range pieces {
		i, pc := i, pc
		g.Go(func() error {
			n, err := f.writeChunk(ctx, off+int64(pc.start), p[pc.start:pc.start+pc.n])
			written[i] = n
			return err
		})
	}
	err := g.Wait()

	total := 0
	for _, n := range written {
		total += n
	}
	return total, err
}

// ReadFrom streams all of r into the fork starting at offset 0, in windows
// that each pipeline up to maxParallel writes. It implements io.ReaderFrom.
func (f *Fork) ReadFrom(r io.Reader) (int64, error) {
	return f.readFrom(context.Background(), r)
}

// ReadFromContext is ReadFrom with a caller-supplied context.
func (f *Fork) ReadFromContext(ctx context.Context, r io.Reader) (int64, error) {
	return f.readFrom(ctx, r)
}

func (f *Fork) readFrom(ctx context.Context, r io.Reader) (int64, error) {
	buf := make([]byte, f.writeChunkSize()*f.maxParallel)
	var total int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if _, werr := f.WriteAt(ctx, buf[:n], total); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// Truncate sets the fork's length (FPSetForkParms), extending or
// shrinking it as needed.
//
// For lengths under 4 GiB it uses the non-extended data-fork-length bit,
// which every server accepts; netatalk in particular rejects the extended
// bit here. Larger lengths require the extended bit.
func (f *Fork) Truncate(ctx context.Context, size int64) error {
	var w builder
	w.u8(cmdSetForkParms)
	w.u8(0)
	w.u16(f.ID)
	if size <= 0xFFFFFFFF {
		w.u16(kFPDataForkLenBit)
		w.u32(uint32(size))
	} else {
		w.u16(kFPExtDataForkLenBit)
		w.u64(uint64(size))
	}

	_, code, err := f.v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("truncate", code)
}
