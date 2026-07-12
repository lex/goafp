package afp

import (
	"context"
	"fmt"
	"sort"

	"github.com/lex/goafp/internal/dsi"
)

// Session is an authenticated AFP session over a DSI connection.
type Session struct {
	conn *dsi.Conn

	// Version is the negotiated AFP version string, e.g. "AFP3.3".
	Version string
}

// NewSession wraps an open DSI connection. Call Login before issuing
// volume operations.
func NewSession(conn *dsi.Conn) *Session {
	return &Session{conn: conn}
}

// command issues a DSICommand request and returns the reply payload and
// AFP result code.
func (s *Session) command(ctx context.Context, payload []byte) ([]byte, Result, error) {
	r, err := s.conn.Request(ctx, dsi.CmdCommand, payload)
	if err != nil {
		return nil, 0, err
	}
	return r.Payload, Result(int32(r.Header.ErrCode)), nil
}

// commandWrite issues a DSIWrite request, used for FPWriteExt. dataOffset
// is the position of the bulk write data within payload.
func (s *Session) commandWrite(ctx context.Context, dataOffset uint32, payload []byte) ([]byte, Result, error) {
	r, err := s.conn.RequestWrite(ctx, dataOffset, payload)
	if err != nil {
		return nil, 0, err
	}
	return r.Payload, Result(int32(r.Header.ErrCode)), nil
}

// serverQuantum is the largest request payload the server accepts (from
// DSIOpenSession); it caps write chunk sizes. Zero means unknown.
func (s *Session) serverQuantum() uint32 { return s.conn.ServerQuantum }

// versionPreference lists supported AFP versions, most preferred first.
// UTF-8 pathnames require AFP 3.x, so 2.x is not supported.
var versionPreference = []string{"AFP3.4", "AFP3.3", "AFP3.2", "AFP3.1"}

func pickVersion(offered []string) (string, error) {
	have := make(map[string]bool, len(offered))
	for _, v := range offered {
		have[v] = true
	}
	for _, v := range versionPreference {
		if have[v] {
			return v, nil
		}
	}
	return "", fmt.Errorf("afp: no supported AFP version (server offers %v; AFP 3.1+ required)", offered)
}

// Login negotiates the AFP version and authenticates. With an empty
// username it performs a guest login ("No User Authent"); otherwise it
// prefers DHX2 and falls back to cleartext only if the server offers no
// stronger option.
func (s *Session) Login(ctx context.Context, info *ServerInfo, username, password string) error {
	version, err := pickVersion(info.AFPVersions)
	if err != nil {
		return err
	}
	s.Version = version

	uams := make(map[string]bool, len(info.UAMs))
	for _, u := range info.UAMs {
		uams[u] = true
	}

	switch {
	case username == "":
		if !uams[uamNoAuth] {
			return fmt.Errorf("afp: server does not allow guest login (UAMs: %v)", info.UAMs)
		}
		return s.loginGuest(ctx)
	case uams[uamDHX2]:
		return s.loginDHX2(ctx, username, password)
	case uams[uamCleartext]:
		return s.loginCleartext(ctx, username, password)
	default:
		return fmt.Errorf("afp: no mutually supported UAM (server offers %v)", info.UAMs)
	}
}

// login sends FPLogin with the given UAM and user auth block.
func (s *Session) login(ctx context.Context, uam string, authinfo []byte) ([]byte, Result, error) {
	var w builder
	w.u8(cmdLogin)
	w.pascal(s.Version)
	w.pascal(uam)
	w.bytes(authinfo)
	return s.command(ctx, w.b)
}

// loginCont sends FPLoginCont for multi-step UAMs.
func (s *Session) loginCont(ctx context.Context, id uint16, authinfo []byte) ([]byte, Result, error) {
	var w builder
	w.u8(cmdLoginCont)
	w.u8(0)
	w.u16(id)
	w.bytes(authinfo)
	return s.command(ctx, w.b)
}

// Logout ends the authenticated session.
func (s *Session) Logout(ctx context.Context) error {
	_, code, err := s.command(ctx, []byte{cmdLogout, 0})
	if err != nil {
		return err
	}
	return resultErr("logout", code)
}

// VolumeInfo is one entry from the server's volume list.
type VolumeInfo struct {
	Name        string
	HasPassword bool
}

// ListVolumes returns the volumes the server exports (FPGetSrvrParms).
func (s *Session) ListVolumes(ctx context.Context) ([]VolumeInfo, error) {
	payload, code, err := s.command(ctx, []byte{cmdGetSrvrParms, 0})
	if err != nil {
		return nil, err
	}
	if err := resultErr("list volumes", code); err != nil {
		return nil, err
	}

	r := &reader{b: payload}
	r.u32("server time")
	n := int(r.u8("volume count"))
	vols := make([]VolumeInfo, 0, n)
	for i := 0; i < n; i++ {
		flags := r.u8("volume flags")
		name := r.pascal("volume name")
		if r.err != nil {
			return nil, r.err
		}
		vols = append(vols, VolumeInfo{
			Name:        name,
			HasPassword: flags&0x80 != 0,
		})
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	return vols, nil
}

// Volume bitmap bits (FPOpenVol / FPGetVolParms).
const (
	kFPVolAttributeBit = 0x0001
	kFPVolIDBit        = 0x0020
)

// Volume attribute bits.
const (
	volAttrReadOnly     = 0x01
	volAttrSupportsUnix = 0x20
	volAttrSupportsUTF8 = 0x40
)

// Volume is an open AFP volume.
type Volume struct {
	s          *Session
	ID         uint16
	Name       string
	Attributes uint16
}

// SupportsUnixPrivs reports whether the volume carries unix permissions.
func (v *Volume) SupportsUnixPrivs() bool {
	return v.Attributes&volAttrSupportsUnix != 0
}

// OpenVolume opens a volume by name (FPOpenVol).
func (s *Session) OpenVolume(ctx context.Context, name string) (*Volume, error) {
	var w builder
	w.u8(cmdOpenVol)
	w.u8(0)
	w.u16(kFPVolAttributeBit | kFPVolIDBit)
	w.pascal(name)

	payload, code, err := s.command(ctx, w.b)
	if err != nil {
		return nil, err
	}
	if err := resultErr("open volume "+name, code); err != nil {
		return nil, err
	}

	// Reply: bitmap, then the requested parameters in bit order.
	r := &reader{b: payload}
	bitmap := r.u16("volume bitmap")
	vol := &Volume{s: s, Name: name}
	if bitmap&kFPVolAttributeBit != 0 {
		vol.Attributes = r.u16("volume attributes")
	}
	if bitmap&kFPVolIDBit != 0 {
		vol.ID = r.u16("volume id")
	}
	if r.err != nil {
		return nil, r.err
	}
	return vol, nil
}

// Close closes the volume (FPCloseVol).
func (v *Volume) Close(ctx context.Context) error {
	var w builder
	w.u8(cmdCloseVol)
	w.u8(0)
	w.u16(v.ID)
	_, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("close volume", code)
}

func (v *Volume) entryBitmaps() (fileBitmap, dirBitmap uint16) {
	fileBitmap = kFPParentDirIDBit | kFPCreateDateBit | kFPModDateBit |
		kFPNodeIDBit | kFPUTF8NameBit | kFPExtDataForkLenBit
	dirBitmap = kFPParentDirIDBit | kFPCreateDateBit | kFPModDateBit |
		kFPNodeIDBit | kFPUTF8NameBit | kFPOffspringCountBit
	if v.SupportsUnixPrivs() {
		fileBitmap |= kFPUnixPrivsBit
		dirBitmap |= kFPUnixPrivsBit
	}
	return fileBitmap, dirBitmap
}

// enumerateBatch is how many entries we request per FPEnumerateExt2 round
// trip. (The C client used 20; servers are happy returning far more.)
const enumerateBatch = 512

// enumerateReplySize is the maximum reply size we advertise per batch.
const enumerateReplySize = 256 * 1024

// ReadDir enumerates the directory at path (relative to dirID, normally
// RootDirID) using as few round trips as the server allows.
func (v *Volume) ReadDir(ctx context.Context, dirID uint32, path string) ([]DirEntry, error) {
	fileBitmap, dirBitmap := v.entryBitmaps()
	var all []DirEntry
	start := uint32(1)

	for {
		var w builder
		w.u8(cmdEnumerateExt2)
		w.u8(0)
		w.u16(v.ID)
		w.u32(dirID)
		w.u16(fileBitmap)
		w.u16(dirBitmap)
		w.u16(enumerateBatch)
		w.u32(start)
		w.u32(enumerateReplySize)
		w.path(path)

		payload, code, err := v.s.command(ctx, w.b)
		if err != nil {
			return nil, err
		}
		if code == ResObjectNotFound {
			break // past the last entry
		}
		if err := resultErr("enumerate "+path, code); err != nil {
			return nil, err
		}

		entries, err := parseEnumerateReply(payload)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			break
		}
		all = append(all, entries...)
		start += uint32(len(entries))
	}
	return all, nil
}

func parseEnumerateReply(payload []byte) ([]DirEntry, error) {
	r := &reader{b: payload}
	fileBitmap := r.u16("file bitmap")
	dirBitmap := r.u16("dir bitmap")
	count := int(r.u16("entry count"))
	if r.err != nil {
		return nil, r.err
	}

	entries := make([]DirEntry, 0, count)
	for i := 0; i < count; i++ {
		// Each entry: u16 total length, u8 isdir, u8 pad, params.
		length := int(r.u16("entry length"))
		isDir := r.u8("entry isdir") != 0
		r.u8("entry pad")
		if r.err != nil {
			return nil, r.err
		}
		if length < 4 {
			return nil, fmt.Errorf("afp: enumerate entry %d has bogus length %d", i, length)
		}
		block := r.take(length-4, "entry parameters")
		if r.err != nil {
			return nil, r.err
		}
		bitmap := fileBitmap
		if isDir {
			bitmap = dirBitmap
		}
		e, err := parseEntryParams(block, isDir, bitmap)
		if err != nil {
			return nil, fmt.Errorf("afp: enumerate entry %d: %w", i, err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Stat returns the parameters of a single file or directory
// (FPGetFileDirParms).
func (v *Volume) Stat(ctx context.Context, dirID uint32, path string) (DirEntry, error) {
	fileBitmap, dirBitmap := v.entryBitmaps()

	var w builder
	w.u8(cmdGetFileDirParms)
	w.u8(0)
	w.u16(v.ID)
	w.u32(dirID)
	w.u16(fileBitmap)
	w.u16(dirBitmap)
	w.path(path)

	payload, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return DirEntry{}, err
	}
	if err := resultErr("stat "+path, code); err != nil {
		return DirEntry{}, err
	}

	r := &reader{b: payload}
	replyFileBitmap := r.u16("file bitmap")
	replyDirBitmap := r.u16("dir bitmap")
	isDir := r.u8("isdir") != 0
	r.u8("pad")
	if r.err != nil {
		return DirEntry{}, r.err
	}
	block := payload[r.pos:]
	bitmap := replyFileBitmap
	if isDir {
		bitmap = replyDirBitmap
	}
	return parseEntryParams(block, isDir, bitmap)
}
