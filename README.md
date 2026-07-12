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

Early development, but the read path works end to end against real
netatalk (verified in CI-style integration tests, see below).

- [x] DSI session layer: framing, pipelined request/reply, OpenSession
      quantum negotiation
- [x] `goafp status` — query a server without authenticating
- [x] Login: guest (No User Authent) and DHX2 (Diffie-Hellman + CAST5)
- [x] Volume list/open, directory enumeration, stat, UTF-8 path handling
- [x] File reads with pipelined readahead (concurrent FPReadExt, in-order
      reassembly)
- [x] netatalk-in-Docker integration test suite
- [ ] Cleartext/SRP UAMs
- [ ] Write path (create, write with coalescing, mkdir, rename, delete)
- [ ] NFS bridge mounting (Linux + macOS)

## Usage

```sh
go build ./cmd/goafp

# Query any AFP server (Time Capsule, netatalk, old macOS)
./goafp status myserver.local

# List exported volumes (guest, or user:pass@ for authenticated)
./goafp volumes afp://myserver.local
./goafp volumes afp://alice:secret@myserver.local

# List a directory in a volume
./goafp ls afp://alice:secret@myserver.local/Documents
./goafp ls afp://alice:secret@myserver.local/Documents/subdir

# Read a file to stdout, or download it
./goafp cat afp://alice:secret@myserver.local/Documents/notes.txt
./goafp get afp://alice:secret@myserver.local/Documents/archive.zip
```

## Development

```sh
go test ./...          # unit tests (mock DSI/AFP servers, no network)
go vet ./...
```

### Integration tests

`test/integration/run.sh` spins up netatalk in Docker, seeds a share, and
runs the suite against it (requires Docker):

```sh
./test/integration/run.sh
```

## License

BSD 3-Clause. See LICENSE.
