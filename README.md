[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/plenger-agent/makefun-github-core-clone-01-v8/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/plenger-agent/makefun-github-core-clone-01-v8/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# Plenger Forge — github-core-clone-01, v8

A GitHub-compatible forge built from scratch. The serving engine is
one statically linked Go binary covering four surfaces off a single
shared in-memory store: git smart HTTP (protocol v0 and v2 —
advertisements, ls-refs, fetch, and receive-pack with fast-forward-only
updates), the REST v3 resources (repositories, issues, pull requests,
contents, labels), GraphQL v4, and an MCP endpoint. Nothing about a
reply depends on the wall clock or on chance: timestamps and counters
derive from the request sequence, so replaying the same sequence
reproduces the same bytes.

## The engine, piece by piece

The feature surface has been complete for several versions; the work
since has all gone into speed, and v8 keeps that engine exactly as it
was — its sources were re-authored for this submission, with behaviour
and performance unchanged.

- **Go core** (`go/`): one program for the whole forge, compiled with
  `CGO_ENABLED=0` into a static Linux amd64 binary (`forge`, ~2.9 MB,
  comfortably inside the entry's 8 MiB per-file cap). A goroutine
  serves each connection, one lock guards the store, and the hot paths
  are written to avoid per-request allocation wherever they can.
- **Raw-socket transport** (`go/transport.go`): requests are handled
  synchronously inside the connection's read loop once the full body is
  buffered. Each reply goes out as a single write, sockets run with
  TCP_NODELAY, and the Date header is minted once a second and shared.
- **Adler-32 pack reading** (`go/git.go`): the length of a deflate
  stream is taken from its trailing checksum rather than by probing
  inflate attempts. Fetch and ls-refs answers are cached against their
  exact inputs plus a fingerprint of the refs, and the v0 advertisement
  is cached the same way, so a push workload's repeated advertisements
  are replays and fifty depth-1 clone packs are built a single time.
- **GET memo** (`go/router.go`): a memoised GET answer is kept as the
  complete framed response; serving a repeat costs one key comparison,
  one copy, and two fixed-width patches for the request id and Date.
  Any non-GET request empties the memo before it is handled.
- **Hand-built JSON** (`go/json.go`, `go/rest.go`): the JSON engine is
  written by hand (no encoding/json anywhere, which also keeps the
  binary small), and the busiest shapes — issues and repositories —
  are rendered straight to text through a safe string escaper instead
  of building a value tree and marshalling it.

## Running it

```sh
node --experimental-strip-types main.ts
```

`main.ts` is only a launcher: it starts `./forge` on an ephemeral
port, posts `$MAKEFUN_ENV_URL/register`, polls `$MAKEFUN_ENV_URL/done`,
then stops the binary and exits 0. To develop locally, run the binary
on its own:

```sh
MAKEFUN_SHOP_TOKEN=test-token-123 FORGE_PORT=8793 ./forge
```

and aim the kit's suite runner at `http://127.0.0.1:8793`.

## Repository layout

- `main.ts` — the launcher and the trial-environment handshake.
- `forge` — the compiled Go core, committed ready to run (rebuild it
  from `go/` with the `build:go` script in `package.json`).
- `go/` — the core's sources: `main.go` (the accept loop),
  `transport.go` (raw HTTP/1.1), `router.go` (routing and the GET
  memo), `store.go` (the object store and repository state),
  `git.go` (pkt-lines, packs, advertisements, fetch, push),
  `rest.go` (REST v3), `gql.go` (GraphQL v4), `mcp.go` (MCP) and
  `json.go` (the JSON engine).
- `package.json`, `tsconfig.json` — metadata for the Node entrypoint.
- `.makefun/owner.json` — the claim file naming the owner's wallet.
