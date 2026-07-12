package afp

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// afpEpochDelta converts between the AFP epoch (2000-01-01 00:00:00 UTC,
// signed 32-bit seconds) and the Unix epoch.
const afpEpochDelta = 946684800

func afpDate(v uint32) time.Time {
	return time.Unix(int64(int32(v))+afpEpochDelta, 0)
}

// builder accumulates a big-endian request payload.
type builder struct {
	b []byte
}

func (w *builder) u8(v uint8)     { w.b = append(w.b, v) }
func (w *builder) u16(v uint16)   { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *builder) u32(v uint32)   { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *builder) u64(v uint64)   { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *builder) bytes(v []byte) { w.b = append(w.b, v...) }

// pascal appends a 1-byte-length-prefixed string.
func (w *builder) pascal(s string) {
	if len(s) > 255 {
		s = s[:255]
	}
	w.u8(uint8(len(s)))
	w.b = append(w.b, s...)
}

// evenPad pads with a zero byte if the payload length so far is odd.
func (w *builder) evenPad() {
	if len(w.b)%2 == 1 {
		w.u8(0)
	}
}

// utf8PathHint is the text encoding hint the reference implementations
// send with type-3 (UTF-8) pathnames.
const utf8PathHint = 0x08000103

// path appends an AFP type-3 (UTF-8) pathname. Components separated by
// '/' become null separators on the wire, and the name is converted to
// the decomposed form AFP expects.
func (w *builder) path(p string) {
	p = norm.NFD.String(strings.Trim(p, "/"))
	wire := strings.ReplaceAll(p, "/", "\x00")
	w.u8(kFPUTF8Name)
	w.u32(utf8PathHint)
	w.u16(uint16(len(wire)))
	w.b = append(w.b, wire...)
}

// reader walks a big-endian reply payload with bounds checking: after any
// failure, ok() is false and every accessor returns zero values.
type reader struct {
	b   []byte
	pos int
	err error
}

func (r *reader) fail(what string) {
	if r.err == nil {
		r.err = fmt.Errorf("afp: truncated reply: %s at offset %d of %d", what, r.pos, len(r.b))
	}
}

func (r *reader) take(n int, what string) []byte {
	if r.err != nil {
		return nil
	}
	if r.pos+n > len(r.b) {
		r.fail(what)
		return nil
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v
}

func (r *reader) u8(what string) uint8 {
	v := r.take(1, what)
	if v == nil {
		return 0
	}
	return v[0]
}

func (r *reader) u16(what string) uint16 {
	v := r.take(2, what)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint16(v)
}

func (r *reader) u32(what string) uint32 {
	v := r.take(4, what)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint32(v)
}

func (r *reader) u64(what string) uint64 {
	v := r.take(8, what)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

func (r *reader) pascal(what string) string {
	n := r.u8(what)
	v := r.take(int(n), what)
	return string(v)
}

// pascalAt reads a 1-byte-length-prefixed string at an absolute offset
// without moving the cursor.
func pascalAt(b []byte, off int) (string, error) {
	if off < 0 || off >= len(b) {
		return "", fmt.Errorf("afp: string offset %d outside %d-byte block", off, len(b))
	}
	n := int(b[off])
	if off+1+n > len(b) {
		return "", fmt.Errorf("afp: string at %d overruns block", off)
	}
	return string(b[off+1 : off+1+n]), nil
}

// pascal2At reads a 2-byte-length-prefixed string at an absolute offset.
func pascal2At(b []byte, off int) (string, error) {
	if off < 0 || off+2 > len(b) {
		return "", fmt.Errorf("afp: string offset %d outside %d-byte block", off, len(b))
	}
	n := int(binary.BigEndian.Uint16(b[off : off+2]))
	if off+2+n > len(b) {
		return "", fmt.Errorf("afp: string at %d overruns block", off)
	}
	return string(b[off+2 : off+2+n]), nil
}
