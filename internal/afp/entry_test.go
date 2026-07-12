package afp

import (
	"encoding/binary"
	"testing"
	"time"
)

const (
	testFileBitmap = kFPParentDirIDBit | kFPCreateDateBit | kFPModDateBit |
		kFPNodeIDBit | kFPUTF8NameBit | kFPExtDataForkLenBit
	testDirBitmap = kFPParentDirIDBit | kFPCreateDateBit | kFPModDateBit |
		kFPNodeIDBit | kFPUTF8NameBit | kFPOffspringCountBit
)

func wireDate(t time.Time) uint32 {
	return uint32(int32(t.Unix() - afpEpochDelta))
}

// buildEntryParams lays out a parameter block the way servers do: fixed
// fields in bitmap-bit order, with the UTF-8 name (hint + 2-byte-length
// string) appended at the end and referenced by offset.
func buildEntryParams(isDir bool, name string, nodeID uint32, size uint64, offspring uint16, mod time.Time) []byte {
	var w builder
	w.u32(7)             // parent dir id
	w.u32(wireDate(mod)) // create date (reuse mod for the test)
	w.u32(wireDate(mod)) // mod date
	w.u32(nodeID)        // node id
	if isDir {
		w.u16(offspring)
	} else {
		w.u64(size)
	}
	nameOff := len(w.b) + 2 + 4 // after the offset field and its pad
	w.u16(uint16(nameOff))
	w.u32(0) // pad
	// Name blob: 4-byte encoding hint, 2-byte length, bytes.
	w.u32(utf8PathHint)
	w.u16(uint16(len(name)))
	w.bytes([]byte(name))
	return w.b
}

func buildEnumerateReply(entries ...[]byte) []byte {
	var w builder
	w.u16(testFileBitmap)
	w.u16(testDirBitmap)
	w.u16(uint16(len(entries)))
	for _, e := range entries {
		w.bytes(e)
	}
	return w.b
}

func entryBlock(isDir bool, params []byte) []byte {
	length := 4 + len(params)
	pad := length % 2
	b := make([]byte, 4, length+pad)
	binary.BigEndian.PutUint16(b[0:2], uint16(length+pad))
	if isDir {
		b[2] = 1
	}
	b = append(b, params...)
	if pad == 1 {
		b = append(b, 0)
	}
	return b
}

func TestParseEnumerateReply(t *testing.T) {
	mod := time.Unix(1700000000, 0)
	reply := buildEnumerateReply(
		entryBlock(false, buildEntryParams(false, "notes.txt", 101, 4096, 0, mod)),
		entryBlock(true, buildEntryParams(true, "Docs", 102, 0, 3, mod)),
	)

	entries, err := parseEnumerateReply(reply)
	if err != nil {
		t.Fatalf("parseEnumerateReply: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	f := entries[0]
	if f.IsDir || f.Name != "notes.txt" || f.Size != 4096 || f.NodeID != 101 {
		t.Errorf("file entry = %+v", f)
	}
	if !f.ModTime.Equal(mod) {
		t.Errorf("file mod time = %v, want %v", f.ModTime, mod)
	}
	d := entries[1]
	if !d.IsDir || d.Name != "Docs" || d.Offspring != 3 || d.NodeID != 102 {
		t.Errorf("dir entry = %+v", d)
	}
}

func TestParseEnumerateReplyDecomposedName(t *testing.T) {
	// AFP sends decomposed UTF-8 ("A" + combining ring above, U+030A);
	// the parser must return the precomposed form (U+00C5).
	mod := time.Unix(1700000000, 0)
	decomposed := "A\u030Arbok"
	precomposed := "\u00C5rbok"
	reply := buildEnumerateReply(
		entryBlock(true, buildEntryParams(true, decomposed, 5, 0, 0, mod)),
	)
	entries, err := parseEnumerateReply(reply)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Name != precomposed {
		t.Errorf("name = %q, want precomposed %q", entries[0].Name, precomposed)
	}
}

func TestParseEnumerateReplyMalformed(t *testing.T) {
	mod := time.Unix(1700000000, 0)
	good := buildEnumerateReply(
		entryBlock(false, buildEntryParams(false, "x", 1, 2, 0, mod)),
	)
	// No truncation may panic; errors are expected once fields go missing.
	for i := range good {
		_, _ = parseEnumerateReply(good[:i])
	}
	// A name offset pointing outside the block must error, not read OOB.
	bad := append([]byte(nil), good...)
	// Entry params start at 6 (reply header) + 4 (entry header); the
	// UTF-8 name offset field sits after 24 bytes of fixed fields.
	binary.BigEndian.PutUint16(bad[6+4+24:], 0xFFF0)
	if _, err := parseEnumerateReply(bad); err == nil {
		t.Error("out-of-range name offset: want error")
	}
}

func TestPathEncoding(t *testing.T) {
	var w builder
	w.path("Docs/\u00C5rbok") // precomposed Å in the local path
	// Wire format: type byte, 4-byte hint, 2-byte length, then the path
	// with '/' as NUL and the name decomposed.
	if w.b[0] != kFPUTF8Name {
		t.Errorf("path type = %d", w.b[0])
	}
	got := string(w.b[7:])
	want := "Docs\x00A\u030Arbok"
	if got != want {
		t.Errorf("wire path = %q, want %q", got, want)
	}
}
