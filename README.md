# goafp

A modern Apple Filing Protocol (AFP) client in Go, for Linux and macOS.

Apple has deprecated and is removing the AFP client from macOS, leaving
Time Capsules and older AFP-only NAS devices unreachable. goafp is a
from-scratch replacement informed by the original
[afpfs-ng](https://github.com/simonvetter/afpfs-ng) C implementation — see
this [performance analysis](https://github.com/lex/afpfs-ng/blob/master/docs/PERFORMANCE-ANALYSIS.md)
of it for why a rewrite: the C client's synchronous, single-request-at-a-time
architecture made it an order of magnitude slower than the native client,
and fixing that meant rebuilding the core anyway.

## Design

- **Pipelined DSI core** (`internal/dsi`): many requests in flight per
  connection, replies matched to callers by request ID. Concurrency is the
  default, not a retrofit.
- **Pure-Go protocol layer** (`internal/afp`): AFP 3.x commands and reply
  parsing, every offset bounds-checked.
- **Portable mounting (planned)**: goafp will expose mounted volumes as a
  localhost NFSv3 server and auto-mount it via the OS's built-in NFS
  client (`mount_nfs` on macOS, `mount -t nfs` on Linux). One codepath for
  both platforms; no macFUSE kext, no kernel extensions. A native FUSE
  frontend on Linux may follow.
- **Testability**: protocol logic is exercised against in-process mock
  servers with fault injection; performance properties are asserted as
  round-trip counts, not wall-clock times.

## Status

Early development.

- [x] DSI session layer: framing, pipelined request/reply, OpenSession
      quantum negotiation
- [x] `goafp status` — query a server without authenticating
- [ ] Login (No User Authent, DHX2)
- [ ] Volume open, enumerate, getattr
- [ ] File read/write with readahead and write coalescing
- [ ] NFS bridge mounting (Linux + macOS)
- [ ] netatalk-in-Docker integration test suite

## Usage

```sh
go build ./cmd/goafp

# Query any AFP server (Time Capsule, netatalk, old macOS)
./goafp status myserver.local
```

## Development

```sh
go test ./...
go vet ./...
```

## License

BSD 3-Clause. See LICENSE.
