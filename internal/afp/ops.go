package afp

import (
	"context"
	"strings"
)

// File creation flags (FPCreateFile / FPCreateDir).
const (
	createSoft = 0x00 // fail if the object already exists
	createHard = 0x80 // overwrite an existing file
)

// CreateFile creates an empty file at path (relative to dirID). If
// overwrite is false it fails when the file already exists.
func (v *Volume) CreateFile(ctx context.Context, dirID uint32, path string, overwrite bool) error {
	flag := byte(createSoft)
	if overwrite {
		flag = createHard
	}
	var w builder
	w.u8(cmdCreateFile)
	w.u8(flag)
	w.u16(v.ID)
	w.u32(dirID)
	w.path(path)

	_, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("create "+path, code)
}

// Mkdir creates a directory at path (relative to dirID).
func (v *Volume) Mkdir(ctx context.Context, dirID uint32, path string) error {
	var w builder
	w.u8(cmdCreateDir)
	w.u8(0)
	w.u16(v.ID)
	w.u32(dirID)
	w.path(path)

	_, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("mkdir "+path, code)
}

// Delete removes the file or empty directory at path (relative to dirID).
func (v *Volume) Delete(ctx context.Context, dirID uint32, path string) error {
	var w builder
	w.u8(cmdDelete)
	w.u8(0)
	w.u16(v.ID)
	w.u32(dirID)
	w.path(path)

	_, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("delete "+path, code)
}

// Rename moves and/or renames the object at from to to; both paths are
// relative to dirID. It uses FPMoveAndRename, which (unlike FPRename)
// netatalk and macOS accept for both same-directory renames and moves
// across directories.
func (v *Volume) Rename(ctx context.Context, dirID uint32, from, to string) error {
	destDir, newName := splitPath(to)

	var w builder
	w.u8(cmdMoveAndRename)
	w.u8(0)
	w.u16(v.ID)
	w.u32(dirID)    // source parent DID
	w.u32(dirID)    // destination parent DID
	w.path(from)    // source path
	w.path(destDir) // destination directory (empty == dirID itself)
	w.path(newName) // new leaf name

	_, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("rename "+from, code)
}

// splitPath separates a slash-separated AFP path into its parent
// directory and final component.
func splitPath(p string) (dir, base string) {
	p = strings.Trim(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}
