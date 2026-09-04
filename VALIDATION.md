# Validation

The project targets Go 1.27.1 on macOS and Linux. The runtime, installer, and
tests are Go-only. Tests cover the peer wire format and persisted SQLite schema
directly, without a legacy runtime.

## Automated checks

`make check` runs formatting checks, `go vet`, pinned `golangci-lint`, and
`go test -race ./...`. `make cross-build` builds CGO-free binaries for macOS and
Linux on arm64 and amd64. GitHub Actions runs the suite on macOS and Linux.

Coverage includes:
- Shared-server shutdown: live and disabled-messaging owners, unregistered socket
  clients, active background threads, process identity mismatches, launch locking,
  graceful stop/restart, repeated shutdown, and refusal to force-kill on timeout.

- Unicode and closing-tag round trips, byte/UTF-16 limits, bounded framing,
  unsafe paths, symlinks, and kernel-verified Unix socket credentials.
- Durable IDs, persisted-schema compatibility, repeated reads, byte-bounded pages,
  cursors, deduplication, rate limits, queue bounds, expiry, and correlated statuses.
- Concurrent acceptance and acknowledgement, partial acknowledgement, races
  between acknowledgement and notice completion, and persistent pending attempts.
- Real worker processes: discovery, send/reply, admission, acceptance/read
  receipts, and restart persistence.
- Default `accept` admission and `bypass` envelopes, cross-class replies,
  explicit CLI/MCP overrides, and hooks preserving messaging settings across
  Codex approval modes and worker restarts.
- Concurrent duplicate-name rejection, rename rollback on a name collision,
  preservation of client ownership across worker restarts, cleanup on owner
  exit, PID reuse rejection, and hooks refusing to create unowned workers.
- Immutable installation, arbitrary working directories, quoted paths, and
  refusal to overwrite unrelated files.
- Exact app-server thread binding, idle/active tool output, dropped connections,
  ambiguous writes without replay, history reconciliation, and rejected requests.

## Installed Codex smoke test

```sh
CSC_TEST_CODEX=1 GOTOOLCHAIN=go1.27.1 go test ./internal/bridge -run TestLiveCodexAppServer -v
```

This starts the installed Codex app-server with an isolated `CODEX_HOME` and a
local fake Responses endpoint. No real model credentials or model calls are used.
It verifies new-thread persistence, idle wake-up, active-turn tool-output delivery,
and receipt recovery after reconnecting. Verified with Codex CLI 0.153.2 on macOS
arm64 during this port.

## Manual messaging check

After restarting the shared app-server and both Codex clients, two Go peers
exchanged one ping/reply followed by five sequential ping/reply pairs. Each
numbered reply arrived before the next ping was sent. All replies were
acknowledged and the receiving inbox was empty afterward. The separate first
ping also returned a verified read receipt.

## Limits

The smoke test confirms app-server behavior, not that an arbitrary model will
follow the inbox notice or finish a peer's task. A CLI acknowledgement is the
processing receipt. Ambiguous notice writes remain latched until reconciled or
the inbox is acknowledged; the bridge does not claim exactly-once delivery.

Current embedded Codex UIs require a manual exit and `launch --resume UUID` to
join the shared backend. Desktop Codex, remote/cloud peers, and native Windows
are outside this implementation. Linux runtime coverage is configured in CI;
local validation on a Mac does not substitute for a completed Linux CI run.
