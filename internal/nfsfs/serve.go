package nfsfs

import (
	"net"

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
	handler := nfshelper.NewNullAuthHandler(fs)
	cached := nfshelper.NewCachingHandler(handler, cacheEntries)
	return nfs.Serve(l, cached)
}
