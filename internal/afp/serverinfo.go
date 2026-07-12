// Package afp implements the Apple Filing Protocol on top of the DSI
// transport.
package afp

import (
	"encoding/binary"
	"fmt"
)

// ServerInfo flag bits (FPGetSrvrInfo).
const (
	FlagSupportsCopyFile     = 1 << 0
	FlagSupportsChgPwd       = 1 << 1
	FlagDontAllowSavePwd     = 1 << 2
	FlagSupportsSrvrMsg      = 1 << 3
	FlagSupportsSrvrSig      = 1 << 4
	FlagSupportsTCP          = 1 << 5
	FlagSupportsSrvrNotify   = 1 << 6
	FlagSupportsReconnect    = 1 << 7
	FlagSupportsDirServices  = 1 << 8
	FlagSupportsUTF8SrvrName = 1 << 9
	FlagSupportsUUIDs        = 1 << 10
	FlagSupportsExtSleep     = 1 << 11
	FlagSupportsSuperClient  = 1 << 15
)

// ServerInfo is the parsed FPGetSrvrInfo reply block, available without
// authentication via DSIGetStatus.
type ServerInfo struct {
	ServerName  string
	MachineType string
	AFPVersions []string
	UAMs        []string
	Flags       uint16
}

// ParseServerInfo parses an FPGetSrvrInfo reply block. All offsets are
// bounds-checked; a malformed block yields an error, never a panic.
func ParseServerInfo(b []byte) (*ServerInfo, error) {
	// Fixed portion: four uint16 offsets, uint16 flags, then the server
	// name as a Pascal string. Offsets are relative to the block start.
	if len(b) < 11 {
		return nil, fmt.Errorf("afp: server info block too short (%d bytes)", len(b))
	}
	info := &ServerInfo{
		Flags: binary.BigEndian.Uint16(b[8:10]),
	}

	var err error
	if info.ServerName, _, err = pascalString(b, 10); err != nil {
		return nil, fmt.Errorf("afp: server name: %w", err)
	}
	if info.MachineType, _, err = pascalString(b, int(binary.BigEndian.Uint16(b[0:2]))); err != nil {
		return nil, fmt.Errorf("afp: machine type: %w", err)
	}
	if info.AFPVersions, err = pascalList(b, int(binary.BigEndian.Uint16(b[2:4]))); err != nil {
		return nil, fmt.Errorf("afp: version list: %w", err)
	}
	if info.UAMs, err = pascalList(b, int(binary.BigEndian.Uint16(b[4:6]))); err != nil {
		return nil, fmt.Errorf("afp: UAM list: %w", err)
	}
	return info, nil
}

// pascalString reads a length-prefixed string at off and returns it along
// with the offset of the first byte past it.
func pascalString(b []byte, off int) (string, int, error) {
	if off < 0 || off >= len(b) {
		return "", 0, fmt.Errorf("offset %d outside %d-byte block", off, len(b))
	}
	n := int(b[off])
	end := off + 1 + n
	if end > len(b) {
		return "", 0, fmt.Errorf("string at %d overruns block (%d+%d > %d)", off, off+1, n, len(b))
	}
	return string(b[off+1 : end]), end, nil
}

// pascalList reads a count byte at off followed by that many Pascal
// strings.
func pascalList(b []byte, off int) ([]string, error) {
	if off <= 0 { // offset 0 means the list is absent
		return nil, nil
	}
	if off >= len(b) {
		return nil, fmt.Errorf("list offset %d outside %d-byte block", off, len(b))
	}
	count := int(b[off])
	items := make([]string, 0, count)
	pos := off + 1
	for i := 0; i < count; i++ {
		s, next, err := pascalString(b, pos)
		if err != nil {
			return nil, fmt.Errorf("list item %d: %w", i, err)
		}
		items = append(items, s)
		pos = next
	}
	return items, nil
}
