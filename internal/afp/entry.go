package afp

import (
	"time"

	"golang.org/x/text/unicode/norm"
)

// File/directory parameter bitmap bits.
const (
	kFPAttributeBit   = 0x0001
	kFPParentDirIDBit = 0x0002
	kFPCreateDateBit  = 0x0004
	kFPModDateBit     = 0x0008
	kFPBackupDateBit  = 0x0010
	kFPFinderInfoBit  = 0x0020
	kFPLongNameBit    = 0x0040
	kFPShortNameBit   = 0x0080
	kFPNodeIDBit      = 0x0100
	kFPUTF8NameBit    = 0x2000
	kFPUnixPrivsBit   = 0x8000
	// Directory-only bits.
	kFPOffspringCountBit = 0x0200
	kFPOwnerIDBit        = 0x0400
	kFPGroupIDBit        = 0x0800
	kFPAccessRightsBit   = 0x1000
	// File-only bits.
	kFPDataForkLenBit    = 0x0200
	kFPRsrcForkLenBit    = 0x0400
	kFPExtDataForkLenBit = 0x0800
	kFPLaunchLimitBit    = 0x1000
	kFPExtRsrcForkLenBit = 0x4000
)

// Path type constants.
const kFPUTF8Name = 3

// RootDirID is the directory ID of a volume's root.
const RootDirID = 2

// UnixPrivs are the AFP 3.x unix privilege parameters.
type UnixPrivs struct {
	UID, GID    uint32
	Permissions uint32
}

// DirEntry is one file or directory as returned by enumeration or
// FPGetFileDirParms.
type DirEntry struct {
	Name        string
	IsDir       bool
	Size        uint64 // data fork length; zero for directories
	NodeID      uint32
	ParentDirID uint32
	Offspring   uint16 // directories: number of children
	CreateTime  time.Time
	ModTime     time.Time
	UnixPrivs   UnixPrivs
	HasUnix     bool
	IsSymlink   bool // detected from the "slnk" Finder info signature
}

// parseEntryParams decodes one bitmapped parameter block. The fields
// appear in bitmap-bit order; name fields hold offsets relative to the
// start of the block.
func parseEntryParams(block []byte, isDir bool, bitmap uint16) (DirEntry, error) {
	e := DirEntry{IsDir: isDir}
	r := &reader{b: block}

	if bitmap&kFPAttributeBit != 0 {
		r.u16("attributes")
	}
	if bitmap&kFPParentDirIDBit != 0 {
		e.ParentDirID = r.u32("parent dir id")
	}
	if bitmap&kFPCreateDateBit != 0 {
		e.CreateTime = afpDate(r.u32("create date"))
	}
	if bitmap&kFPModDateBit != 0 {
		e.ModTime = afpDate(r.u32("mod date"))
	}
	if bitmap&kFPBackupDateBit != 0 {
		r.u32("backup date")
	}
	if bitmap&kFPFinderInfoBit != 0 {
		fi := r.take(32, "finder info")
		// A file whose Finder type is "slnk" is an AFP symbolic link.
		if len(fi) >= 4 && string(fi[:4]) == "slnk" {
			e.IsSymlink = true
		}
	}
	if bitmap&kFPLongNameBit != 0 {
		off := r.u16("long name offset")
		if r.err == nil {
			name, err := pascalAt(block, int(off))
			if err != nil {
				return e, err
			}
			e.Name = name
		}
	}
	if bitmap&kFPShortNameBit != 0 {
		r.u16("short name offset")
	}
	if bitmap&kFPNodeIDBit != 0 {
		e.NodeID = r.u32("node id")
	}
	if isDir {
		if bitmap&kFPOffspringCountBit != 0 {
			e.Offspring = r.u16("offspring count")
		}
		if bitmap&kFPOwnerIDBit != 0 {
			e.UnixPrivs.UID = r.u32("owner id")
		}
		if bitmap&kFPGroupIDBit != 0 {
			e.UnixPrivs.GID = r.u32("group id")
		}
		if bitmap&kFPAccessRightsBit != 0 {
			r.u32("access rights")
		}
	} else {
		if bitmap&kFPDataForkLenBit != 0 {
			e.Size = uint64(r.u32("data fork len"))
		}
		if bitmap&kFPRsrcForkLenBit != 0 {
			r.u32("rsrc fork len")
		}
		if bitmap&kFPExtDataForkLenBit != 0 {
			e.Size = r.u64("ext data fork len")
		}
		if bitmap&kFPLaunchLimitBit != 0 {
			r.u16("launch limit")
		}
	}
	if bitmap&kFPUTF8NameBit != 0 {
		off := r.u16("utf8 name offset")
		r.u32("utf8 name pad")
		if r.err == nil {
			// The offset points at a 4-byte text-encoding hint
			// followed by a 2-byte-length-prefixed string.
			name, err := pascal2At(block, int(off)+4)
			if err != nil {
				return e, err
			}
			// AFP names arrive in decomposed form.
			e.Name = norm.NFC.String(name)
		}
	}
	if bitmap&kFPExtRsrcForkLenBit != 0 && !isDir {
		r.u64("ext rsrc fork len")
	}
	if bitmap&kFPUnixPrivsBit != 0 {
		e.UnixPrivs.UID = r.u32("unix uid")
		e.UnixPrivs.GID = r.u32("unix gid")
		e.UnixPrivs.Permissions = r.u32("unix permissions")
		r.u32("unix ua_permissions")
		e.HasUnix = r.err == nil
	}
	return e, r.err
}
