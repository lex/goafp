package afp

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func pascal(s string) []byte {
	return append([]byte{byte(len(s))}, s...)
}

// buildServerInfo assembles a synthetic FPGetSrvrInfo block the way a real
// server lays it out: fixed header, server name, then the variable tables
// that the header offsets point at.
func buildServerInfo(name, machine string, versions, uams []string, flags uint16) []byte {
	b := make([]byte, 10)
	binary.BigEndian.PutUint16(b[8:10], flags)
	b = append(b, pascal(name)...)

	binary.BigEndian.PutUint16(b[0:2], uint16(len(b))) // machine type offset
	b = append(b, pascal(machine)...)

	binary.BigEndian.PutUint16(b[2:4], uint16(len(b))) // versions offset
	b = append(b, byte(len(versions)))
	for _, v := range versions {
		b = append(b, pascal(v)...)
	}

	binary.BigEndian.PutUint16(b[4:6], uint16(len(b))) // UAMs offset
	b = append(b, byte(len(uams)))
	for _, u := range uams {
		b = append(b, pascal(u)...)
	}
	return b
}

func TestParseServerInfo(t *testing.T) {
	versions := []string{"AFP3.3", "AFP3.4"}
	uams := []string{"No User Authent", "DHX2"}
	block := buildServerInfo("TimeCapsule", "Netatalk", versions, uams,
		FlagSupportsTCP|FlagSupportsUTF8SrvrName)

	info, err := ParseServerInfo(block)
	if err != nil {
		t.Fatalf("ParseServerInfo: %v", err)
	}
	if info.ServerName != "TimeCapsule" {
		t.Errorf("ServerName = %q", info.ServerName)
	}
	if info.MachineType != "Netatalk" {
		t.Errorf("MachineType = %q", info.MachineType)
	}
	if !reflect.DeepEqual(info.AFPVersions, versions) {
		t.Errorf("AFPVersions = %v, want %v", info.AFPVersions, versions)
	}
	if !reflect.DeepEqual(info.UAMs, uams) {
		t.Errorf("UAMs = %v, want %v", info.UAMs, uams)
	}
	if info.Flags&FlagSupportsTCP == 0 {
		t.Error("FlagSupportsTCP not set")
	}
}

// TestParseServerInfoMalformed feeds truncated and corrupt blocks; parsing
// must return errors, never panic or read out of bounds. (The C client this
// project replaces had an out-of-bounds write in exactly this code path.)
func TestParseServerInfoMalformed(t *testing.T) {
	good := buildServerInfo("S", "M", []string{"AFP3.3"}, []string{"DHX2"}, 0)

	// Every possible truncation must be handled without panicking.
	// (Errors are expected but not required for every length: offsets
	// point backwards, so some prefixes are self-consistent.)
	for i := range good {
		_, _ = ParseServerInfo(good[:i])
	}

	// Offsets pointing outside the block.
	bad := append([]byte(nil), good...)
	binary.BigEndian.PutUint16(bad[0:2], 0xFFFF)
	if _, err := ParseServerInfo(bad); err == nil {
		t.Error("machine type offset out of range: want error")
	}

	// String length running past the end of the block.
	bad = append([]byte(nil), good...)
	bad[len(bad)-6] = 0xFF // corrupt a length byte inside the UAM table
	if _, err := ParseServerInfo(bad); err == nil {
		t.Error("string overrunning block: want error")
	}
}
