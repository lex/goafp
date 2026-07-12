package afp

import (
	"context"
	"time"
)

// writeParamsHeader writes the common FPSetFileDirParms preamble (command,
// volume, dir id, bitmap, path) and pads to the even boundary AFP expects
// before the parameter data block.
func (v *Volume) writeParamsHeader(bitmap uint16, dirID uint32, path string) *builder {
	var w builder
	w.u8(cmdSetFileDirParms)
	w.u8(0)
	w.u16(v.ID)
	w.u32(dirID)
	w.u16(bitmap)
	w.path(path)
	w.evenPad()
	return &w
}

// SetUnixPrivs sets the owner, group, and mode of the object at path
// (relative to dirID). mode should include the file-type bits (S_IFREG /
// S_IFDIR); callers preserving an existing type can OR them in.
func (v *Volume) SetUnixPrivs(ctx context.Context, dirID uint32, path string, uid, gid, mode uint32) error {
	w := v.writeParamsHeader(kFPUnixPrivsBit, dirID, path)
	w.u32(uid)
	w.u32(gid)
	w.u32(mode)
	w.u32(0) // ua_permissions (access rights) — left unset

	_, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("set unix privs "+path, code)
}

// SetModTime sets the modification date of the object at path.
func (v *Volume) SetModTime(ctx context.Context, dirID uint32, path string, mtime time.Time) error {
	w := v.writeParamsHeader(kFPModDateBit, dirID, path)
	w.u32(uint32(int32(mtime.Unix() - afpEpochDelta)))

	_, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("set mod time "+path, code)
}

// FSInfo describes a volume's capacity (FPGetVolParms).
type FSInfo struct {
	TotalBytes uint64
	FreeBytes  uint64
	BlockSize  uint32
}

// StatFS returns the volume's capacity using the AFP 3.x extended fields.
func (v *Volume) StatFS(ctx context.Context) (FSInfo, error) {
	var w builder
	w.u8(cmdGetVolParms)
	w.u8(0)
	w.u16(v.ID)
	w.u16(kFPVolExtBytesFreeBit | kFPVolExtBytesTotalBit | kFPVolBlockSizeBit)

	payload, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return FSInfo{}, err
	}
	if err := resultErr("statfs", code); err != nil {
		return FSInfo{}, err
	}

	// Reply: bitmap, then the requested fields in ascending bit order.
	r := &reader{b: payload}
	r.u16("volume bitmap")
	info := FSInfo{
		FreeBytes:  r.u64("ext bytes free"),
		TotalBytes: r.u64("ext bytes total"),
		BlockSize:  r.u32("block size"),
	}
	if r.err != nil {
		return FSInfo{}, r.err
	}
	return info, nil
}
