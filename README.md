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
- **Portable mounting**: `goafp mount` exposes a volume as a localhost
  NFSv3 server (`internal/nfsfs`, a go-billy adapter over the AFP layer)
  that the OS mounts with its built-in NFS client — `mount_nfs` on macOS,
  `mount -t nfs` on Linux. One codepath for both platforms; no macFUSE
  kext, no kernel extensions. The billy adapter is protocol-agnostic, so
  an SMB frontend could reuse the same core.
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
- [x] Write path: create, pipelined/coalesced writes, truncate, mkdir,
      rename/move, delete
- [x] NFS bridge mounting for Linux + macOS (`goafp mount`)
- [x] netatalk-in-Docker integration test suite (incl. end-to-end NFS)
- [ ] Cleartext/SRP UAMs
- [ ] chmod/chown/utimes mapped through the bridge (currently no-ops)
- [ ] Symlink support over the bridge

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

# Upload, create directories, rename/move, delete
./goafp put ./report.pdf afp://alice:secret@myserver.local/Documents/report.pdf
./goafp mkdir afp://alice:secret@myserver.local/Documents/2026
./goafp mv    afp://alice:secret@myserver.local/Documents/report.pdf 2026/report.pdf
./goafp rm    afp://alice:secret@myserver.local/Documents/2026/report.pdf
```

### Mounting a volume

`goafp mount` serves a volume over NFS on localhost; mount it with the
OS's built-in NFS client (no kernel extension needed):

```sh
# Terminal 1: serve the volume (stays running)
./goafp mount afp://alice:secret@myserver.local/Documents

# Terminal 2: mount it (goafp prints the exact command for your OS)
#   macOS:
sudo mount_nfs -o vers=3,tcp,port=2049,mountport=2049,noowners,resvport \
    127.0.0.1:/ /path/to/mountpoint
#   Linux:
sudo mount -t nfs -o vers=3,tcp,port=2049,mountport=2049,nolock \
    127.0.0.1:/ /path/to/mountpoint
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
