package afp

import (
	"context"
	"io"
)

// symlinkFinderInfo is the 32-byte Finder info that marks a file as an AFP
// symbolic link: Finder type "slnk", creator "rhap" (the historical
// Rhapsody/AFP convention), the rest zero.
func symlinkFinderInfo() [32]byte {
	var fi [32]byte
	copy(fi[0:4], "slnk")
	copy(fi[4:8], "rhap")
	return fi
}

// setFinderInfo sets the 32-byte Finder info of the object at path.
func (v *Volume) setFinderInfo(ctx context.Context, dirID uint32, path string, fi [32]byte) error {
	w := v.writeParamsHeader(kFPFinderInfoBit, dirID, path)
	w.bytes(fi[:])

	_, code, err := v.s.command(ctx, w.b)
	if err != nil {
		return err
	}
	return resultErr("set finder info "+path, code)
}

// Symlink creates a symbolic link at link (relative to dirID) pointing at
// target. AFP represents a symlink as a regular file whose contents are the
// target path and whose Finder type is "slnk".
func (v *Volume) Symlink(ctx context.Context, dirID uint32, link, target string) error {
	if err := v.CreateFile(ctx, dirID, link, false); err != nil {
		return err
	}
	fork, err := v.OpenForkRW(ctx, dirID, link)
	if err != nil {
		return err
	}
	if _, err := fork.WriteAt(ctx, []byte(target), 0); err != nil {
		fork.Close(ctx)
		return err
	}
	if err := fork.Close(ctx); err != nil {
		return err
	}
	return v.setFinderInfo(ctx, dirID, link, symlinkFinderInfo())
}

// ReadLink returns the target of the symbolic link at path: the file's
// contents, which for a symlink is the target path.
func (v *Volume) ReadLink(ctx context.Context, dirID uint32, path string) (string, error) {
	fork, err := v.OpenFork(ctx, dirID, path)
	if err != nil {
		return "", err
	}
	defer fork.Close(ctx)

	if fork.Size == 0 {
		return "", nil
	}
	buf := make([]byte, fork.Size)
	n, err := fork.ReadAt(ctx, buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	return string(buf[:n]), nil
}
