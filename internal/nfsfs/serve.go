package nfsfs

import (
	"context"
	"net"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// Serve serves the filesystem as NFSv3 (plus the MOUNT protocol) on the
// given listener until it is closed. Both programs are multiplexed on the
// single TCP port the listener is bound to.
//
// cacheEntries bounds the handler's file-handle cache; a few thousand is
// plenty for interactive use.
func Serve(l net.Listener, fs *FS, cacheEntries int) error {
	base := nfshelper.NewNullAuthHandler(fs)
	handler := &statfsHandler{Handler: base, fs: fs}
	cached := nfshelper.NewCachingHandler(handler, cacheEntries)
	return nfs.Serve(l, cached)
}

// statfsHandler wraps the null-auth handler to answer FSStat from the live
// AFP volume rather than reporting nothing.
type statfsHandler struct {
	nfs.Handler
	fs *FS
}

func (h *statfsHandler) FSStat(ctx context.Context, _ billy.Filesystem, s *nfs.FSStat) error {
	info, err := h.fs.vol.StatFS(h.fs.ctx)
	if err != nil {
		// Non-fatal: report unknown capacity rather than failing the mount.
		return nil
	}
	s.TotalSize = info.TotalBytes
	s.FreeSize = info.FreeBytes
	s.AvailableSize = info.FreeBytes
	return nil
}
