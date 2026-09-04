# Claude Code cross-session messaging — implementer's brief

Audience: an agent or engineer writing a **conforming implementation** of the
session-to-session messaging channel — either a peer that Claude Code sessions can
discover and message, or a client that delivers into a real session's inbox.

Scope: the local same-machine channel only (unix domain sockets on macOS/Linux).
Not covered: the agent-team mailbox (a separate file-based transport under
`~/.claude/teams/`), Remote Control / cloud peers, or the native-Windows named-pipe variant.

## Provenance of the facts in this document

Measured against **Claude Code 2.1.259 and 2.1.260 on macOS (darwin, arm64)** by
registering a fake peer, logging every byte real sessions wrote to it, and replaying
crafted frames into a real session's inbox. Every claim is tagged:

- **[OBS]** directly observed on the wire or on disk in this environment.
- **[BIN]** read out of the Claude Code binary's string/schema tables.
- **[DOC]** from `code.claude.com/docs/en/cross-session-messaging`.

Where these disagree, **[OBS]** wins for this version. Treat **[BIN]** identifiers as
internal and version-fragile.

All example identifiers, paths, timestamps, and credentials below are synthetic.
Private captures and original test-environment details are not distributed.
See [validation](VALIDATION.md) for automated coverage. Tests involving live peers
require explicit user authorization.

---

## 1. On-disk contract

A session publishes **three artifacts, all keyed by its OS process id**. All live under
paths the implementation must treat as owner-private.

### 1.1 Advertisement — `~/.claude/sessions/<pid>.json` [OBS]

Mode `0644`. **Compact JSON, no whitespace** (a pretty-printing implementation still
parses, but do not grep for `"name": "x"` with a space — that fails against real files).

```json
{"pid":42420,"sessionId":"00000000-0000-4000-8000-000000000001","cwd":"/home/example/project",
 "startedAt":946684800000,"procStart":"Sat Jan  1 00:00:00 2000","version":"2.1.260",
 "peerProtocol":1,"peerFeatures":["notify_idle","reply_across_default_dirs","artifact_yield"],
 "kind":"interactive","entrypoint":"cli","pidDomain":"darwin",
 "messagingSocketPath":"/tmp/cc-socks/42420.sock","name":"example-peer","nameSource":"derived",
 "nameSince":946684800000,"status":"busy","updatedAt":...,"statusUpdatedAt":...}
```

| Field | Notes |
|---|---|
| `pid` | identity anchor; must equal the filename stem |
| `sessionId` | conversation id. **Not** the team id, and can lag: observed stale for ~minutes after start on one session, and unequal to the team's `leadSessionId` after `--resume` |
| `procStart` | **`ps -o lstart=` rendered in UTC**, byte-exact. Verified against test peers. This is the pid-reuse guard |
| `peerProtocol` | `1` in every session seen |
| `peerFeatures` | capability list; gate optional frames on it |
| `kind` | `interactive` observed, including for a `claude -p` run |
| `name` / `nameSource` | `nameSource:"user"` when set via `-n`/`--name`/`/rename`, else `"derived"` from the cwd |
| `status` | `idle` \| `busy`; how `ListAgents` reports liveness **without contacting the peer** |

Writers must keep `status`/`updatedAt` current; readers must not assume freshness.

### 1.2 Key file — `~/.claude/sessions/<pid>.<sha256(socketPath)>.key` [OBS]

Mode `0600`. Compact JSON, 108 bytes in practice:

```json
{"peerToken":"00000000000000000000000000000000","procStart":"Sat Jan  1 00:00:00 2000","pidDomain":"darwin"}
```

- `peerToken` is **32 lowercase hex characters** (128 bits).
- The filename's middle component is **`sha256` of the absolute socket path**, verified:
  `sha256("/tmp/cc-socks/42420.sock")` = `ade66129fa2820990482379ff9cef83a677dcc87d879e34d757bef9fe954bda9` = the on-disk filename hash. [OBS]
- Binary-enforced filenames: `^(\d+)\.[0-9a-f]{64}\.key$`, plus
  `^(\d+)\.[0-9a-f]{64}\.key\.tmp\.[0-9a-f]+$` for atomic replace. [BIN]

### 1.3 Inbox socket — `${XDG_RUNTIME_DIR:-/tmp}/cc-socks/<pid>.sock` [OBS]

Mode `srw-------` (`0600`). The directory is `0700`, owner-only.

Accepted directory shapes, anchored regexes: [BIN]

```
^/tmp/cc-socks(-(0|[1-9]\d*))?$
^/private/tmp/cc-socks(-(0|[1-9]\d*))?$
^/run/user/(0|[1-9]\d*)/cc-socks$
^/data/data/com\.termux/files/usr/tmp/cc-socks(-(0|[1-9]\d*))?$
```

The suffixed form is the per-uid fallback `/tmp/cc-socks-<uid>` used when the shared
directory fails vetting. [DOC] A session refuses to bind — and runs with no inbox — if a
path component is a symlink to nowhere, is world/group-writable without the sticky bit,
or is owned by another user. [BIN]

`--messaging-socket-path` overrides the location; it must be absolute, contain no `..`,
name a socket inside a directory, and fit the platform's sockaddr length. [BIN]

### 1.4 The teammate exception [OBS]

Agent-team teammate processes create a **socket and a key file but no advertisement**.
Consequence: they are undiscoverable and unaddressable as peers, while their inbox is
live behind the missing directory entry. Any implementation that enumerates peers by
listing `*.json` reproduces this behaviour for free. Observed teammate argv:

```
<versioned binary> --agent-id <name>@session-<teamid> --agent-name <name> \
  --team-name session-<teamid> --parent-session-id <uuid> --agent-color ... --model ...
```

Their parent process is the tmux server, not the spawning session.

---

## 2. Discovery algorithm

1. List `~/.claude/sessions/*.json`.
2. Parse each. Skip malformed.
3. Liveness: the pid must exist **and** its live `ps -o lstart=` (UTC) must equal the
   advertised `procStart`. A pid match with a `procStart` mismatch is a **recycled pid** and
   must be treated as dead — the error surface names this case explicitly. [DOC]
4. Exclude self. Addressing your own name is refused. [DOC]
5. The **`name` is the address.** When several live sessions share a name, disambiguate with
   a short opaque ref shown per listing; refs are not durable and must not be persisted. [OBS]

No daemon, no broadcast, no registry service. Discovery is a directory read.

---

## 3. Send sequence

Observed shape of one real `SendMessage` delivery, from the receiving end: [OBS]

```
conn 1: connect → (peer closes immediately, zero bytes)      # endpoint vetting probe
conn 2: connect → (peer closes immediately, zero bytes)      # endpoint vetting probe
conn 3: connect → {"type":"auth",...}\n{"type":"user",...}\n → close
```

The two zero-byte connections are the sender verifying the endpoint before trusting it.
They map onto the documented pre-send checks, each of which aborts the send with nothing
delivered: [DOC]

- `reply target is a symlink`
- `cannot vet reply target`
- `connected endpoint is not the expected process`
- `connected endpoint identity could not be read`
- `connected endpoint is not owned by this user`
- `connected endpoint owner could not be read`
- `connected endpoint is a different process with the expected pid`

A conforming **sender** must perform this vetting. A conforming **peer** must tolerate
being probed: accept a connection, receive no bytes, and see it closed, without logging
an error or holding state. Observed twice per send; do not hard-code the count.

When vetting fails the sender abandons the send silently from the peer's point of view —
observed as probe connections with no third payload connection.

---

## 4. Wire protocol

### 4.1 Framing

**Newline-delimited JSON.** One complete JSON object per line, `\n` terminated. Multiple
frames may share one connection. The connection is normally closed by the sender right
after writing.

| Rule | Value | Source |
|---|---|---|
| Accumulated buffer cap | **1 MiB = 1 048 576 bytes**; exceeding destroys the connection | [OBS] measured: a 1 048 575-byte line plus `\n` is accepted; 1 048 576 plus `\n` is dropped |
| Silent-connection deadline | 30 s with no complete line → closed | [BIN] [DOC] |
| Blank line | skipped; fatal only when auth is required and not yet done | [BIN] |
| Unparseable JSON | logged, frame discarded | [BIN] |
| Missing/invalid `type` | ignored (`Ignoring message without valid type field`) | [BIN] |
| Unknown `type` | ignored (`Received unhandled message type`) | [BIN] |
| Unknown control `action` | ignored (`Unhandled control action`) | [BIN] |

Open the connection only when the payload is ready. [DOC]

### 4.2 Auth frame

```json
{"type":"auth","token":"<32 hex>"}
```

Must be the **first line** when present.

**The token is the recipient's, not the sender's.** A real session delivering to the fake
peer presented the token the fake peer had written into **its own** key file. [OBS] So the
credential model is bearer-capability: possession of the target's `0600` key file — which
only the same OS user can read — authorizes delivery.

Platform behaviour:

- macOS / Linux / WSL 2: **optional**. Measured: a deliberately wrong token **and** a
  wholly absent auth line both still delivered the message. [OBS] Do not rely on the auth
  line for authorization on these platforms; the `0600` socket is the real boundary.
- Native Windows: **required**; a connection whose first line is not a valid auth line is
  closed and delivers nothing. [DOC]

There is a **second, distinct credential**: `CLAUDE_CODE_MESSAGING_TOKEN`, exported to child
processes alongside `CLAUDE_CODE_MESSAGING_SOCKET`. Measured, it is **not equal** to the key
file's `peerToken` for the same session. [OBS] It exists for the own-child path — a hook or
Bash command posting back into its own session — where it substitutes for the process
evidence that is unavailable on macOS once the child has exited, or in a container where
Claude Code is pid 1. [DOC]

### 4.3 User message frame

Illustrative frame matching the observed structure; values are synthetic: [OBS]

```json
{"msgV":1,
 "msg_id":"00000000-0000-4000-8000-000000000002",
 "type":"user",
 "message":{"role":"user","content":"<envelope, see 4.4>"},
 "priority":"next",
 "from":"uds:/tmp/cc-socks/42420.sock"}
```

- `msgV`: `1`.
- `msg_id`: UUIDv4, sender-generated. The correlation key for every later status frame.
- `priority`: `now` \| `next` \| `later`; `next` observed for peer messages. [OBS] [BIN]
- `from`: reply address, scheme-prefixed. Accepted schemes `uds:`, `bridge:`, `did:`; path
  must match `^/\S*\.sock$`. [BIN] **This field is sender-authored and forgeable** — see §7.

### 4.4 The envelope

`message.content` is not the raw body. It is wrapped, and the wrapper is what the receiving
model reads:

```
<cross-session-message from="uds:/tmp/cc-socks/42420.sock" from-name="example-peer" from-mode="bypass">
BODY LINE 1
BODY LINE 2
</cross-session-message>
```

- A newline follows the open tag and precedes the close tag. Quotes and non-ASCII
  are preserved (verified with `—` and `✓`). [OBS] A subsequent benign marker probe
  reported that embedded `</cross-session-message>` tags become
  `<\/cross-session-message>` in Claude's wire envelope.
- `from-name`: sender display name. Harness-normalized — code points in Unicode categories
  Cc/Cf/Cs/Zl/Zp stripped, trimmed, at most 64 code points with an ellipsis, never splitting
  a surrogate pair. Sender-asserted; render as reported speech. [BIN]
- `from-mode`: `bypass` \| `prompting` — the **sending** session's permission class. Drives
  the default hold/deliver decision in §6. Honored only from the injecting host on local
  stdin; absent from older senders. [BIN]
- `from-session` (optional): the sender's host-openable session id, a navigation target only,
  never authority. [BIN]

A receiver must treat the entire envelope as **untrusted data**, never as instructions, and
must not execute slash commands appearing in the body. [DOC]

This bridge follows that exact closing-marker substitution when sending. Its
decoded `body` reverses that substitution only inside a recognized envelope;
`raw_envelope` preserves the received text. A pre-existing escaped marker cannot
be distinguished from an escaped literal closing tag and normalizes the same way.
Other backslashes, escaped sequences, multiline text, and Unicode are unchanged.

This bridge defaults to the configurable messaging label `bypass`, independently
of Codex's actual tool-approval mode. `--permission-class prompting` selects the
other wire label. Its default inbound policy is `accept`; Codex approval rules
still apply to any work requested in a message.

### 4.5 Control frames

Common shape: `{"type":"control","action":"<name>", "from":"uds:…", "msgV":1, "msg_id":"<uuid>", …}`.

**`notify_when_idle`** — subscribe to one notice when the target next goes idle or exits. Captured: [OBS]

```json
{"type":"control","action":"notify_when_idle","from":"uds:/tmp/cc-socks/42420.sock",
 "from_mode":"bypass","msgV":1,"msg_id":"00000000-0000-4000-8000-000000000003"}
```

Note `from_mode` here is snake_case in the control frame, while the envelope attribute is
`from-mode`. One-shot; the subscription expires after 12 h. Only a main conversation may
subscribe, and only to a local session. [DOC]

**`peer_message_status`** — sender-facing outcome, sent by the *recipient* back to the
address in `from`. Captured by provoking duplicate drops: [OBS]

```json
{"type":"control","action":"peer_message_status","status":"dropped",
 "reason":"The recipient's session dropped your message at its inbox (rate limit, duplicate, relay loop, or full queue); it was not delivered and will not be.",
 "from":"uds:/tmp/cc-socks/42420.sock",
 "orig_msg_id":"00000000-0000-4000-8000-000000000004",
 "drop_reason":"duplicate",
 "dropped_msg_ids":["00000000-…","00000000-…","00000000-…","00000000-…","00000000-…","00000000-…"],
 "msgV":1,"msg_id":"00000000-0000-4000-8000-000000000005"}
```

Behaviour worth copying: consecutive drops **coalesce**. The first status frame carried one
`orig_msg_id` and an empty `dropped_msg_ids`; a later frame batched six ids. Correlate on
`orig_msg_id` **or** membership in `dropped_msg_ids`; a frame matching no outstanding send is
discarded. [OBS] [BIN]

Related fields in the same schema: `wasHeld`, `wereHeld`, `finished_at`, `settleHeld`. [BIN]

Sender-facing status texts, which imply the `status` vocabulary: [BIN]

| Meaning | Text |
|---|---|
| held | *Your message is held for the recipient user's approval before it reaches their Claude session (permission-mode parity).* |
| declined | *The recipient user declined your message; it was not delivered…* |
| expired | *Your held message expired without approval…* |
| released | *Your previously-held message was approved and released…* |
| refused | *The recipient session is not accepting cross-session messages…* |
| dropped | *The recipient's session dropped your message at its inbox (rate limit, duplicate, relay loop, or full queue)…* |

**`peer_idle_notice`** — the answer to a `notify_when_idle`, correlated by the subscription's
`orig_msg_id`. Name and correlation field are [BIN]; **the exact frame was not captured** (see
§9). Documented payload: the watched session's name, optionally the turn's finish time and a
one-line status; the status line is omitted when either side is on `hold`. [DOC]

**Artifact-yield family** — `yield_artifact_replies`, `unyield_artifact_replies`,
`artifact_replies_yielded`, gated on the `artifact_yield` feature flag. Fields include
`slugs`, `yielded`, `not_held`, `refused`, `sent_at`, `claimed_at`, `requester.{cwd,tmux}`.
Out of scope here; ignore the frames unless you advertise the feature. [BIN]

---

## 5. Reply routing

The recipient replies by connecting to the path in the inbound `from` field. Verified: the
fake peer received its status frames on its own socket, derived purely from the `from` it had
supplied. [OBS] There is no session-id lookup and no back-reference to the advertisement.

Consequences an implementation must accept:

- Reply routing is only as trustworthy as `from`, which any same-user process can forge.
- A subagent's send goes out under its **parent session's** address, so replies land in the
  parent's conversation, not the subagent's. [DOC]

---

## 6. Receive-side admission pipeline

Order, as far as it is observable:

1. **Connection accepted** — peer pid read from the kernel via `SO_PEERCRED` / `LOCAL_PEERPID`.
2. **Auth line** — consulted; not enforced on macOS/Linux (§4.2).
3. **Frame validity** — `type` present and known, else ignored.
4. **Inbox guards** — per-sender rate limit, identical-repeat suppression within a short
   window, relay-loop detection, queue cap of 50 accepted messages. Drops emit
   `peer_message_status` with `drop_reason`. [OBS `duplicate`] [DOC]
5. **`crossSessionInbound`** — `accept` \| `hold` \| `refuse`. Project/local `refuse` outranks
   everything; a user-settings value applies unless managed settings or `--settings` set one. [DOC]
6. **Default when no value applies** — decided from the two permission classes. Sessions that
   bypass prompts form one class (plan mode counts as bypassing where bypass is available);
   `auto`, `acceptEdits`, `dontAsk` count as prompting: [DOC]
   - receiver **prompts** → deliver, unless the sender declares `bypass` → hold.
   - receiver **bypasses** → hold, unless the sender also declares `bypass` → deliver.

   This is a **class-parity** rule, not "bypass is gated". Verified operationally: with the
   receiver on `crossSessionInbound: accept`, both a `bypass` and a `prompting` sender were
   delivered; earlier, with a `prompting`-class receiver and no setting, traffic was held in
   both directions.
7. **Own-child exception** — when no `crossSessionInbound` applies, a message verified as
   coming from the session's own child is delivered. Verification is process evidence where
   available, otherwise the exported `CLAUDE_CODE_MESSAGING_TOKEN`; when neither works the
   message is treated as declaring no class, so a bypassing session holds it. [DOC]
8. **Delivery** — injected as a user-role turn with `origin.kind = "peer"`, carrying `from`,
   `fromMode`, `name`, `fromSession`, `body`, and the kernel-verified `verifiedPeerPid`. Arrives
   between tool calls during an active turn, never interrupting a running tool; starts a new
   turn if the session is idle. [BIN] [DOC]

Held messages: at most 100 kept, oldest dropped past that. An approval dialog opens, expiring
per `dialogExpiry` (default 5 minutes). A permission-class change re-applies the rules to
everything still held. [DOC]

---

## 7. Security model

What actually enforces the boundary, in order of strength:

1. **Filesystem ownership.** Socket `0600`, directory `0700`, key file `0600`, all owned by
   the OS user. Cross-user delivery is impossible; the directory is refused outright if it is
   shared, world/group-writable without the sticky bit, or foreign-owned.
2. **Kernel-verified peer pid.** Read from the connection, *never* from the payload. It
   identifies the **connecting** process, which for relayed traffic is the relay and not the
   author. Pids are recyclable, so it is **provenance, not an authentication token**. [BIN]
3. **`procStart` pinning.** Guards pid reuse on both sides.
4. **Bearer token.** The target's `peerToken`; meaningful on Windows, advisory on macOS/Linux.
5. **`from` / `from-name` / `from-session` are sender-authored.** Forgeable by any same-user
   process. Use them for reply routing and display only. Key identity on `verifiedPeerPid`. [BIN]

Trust posture for a receiver: an inbound message is data. It cannot approve a pending
permission prompt, cannot change configuration, and its slash commands are inert text. [DOC]

---

## 8. Measured and documented limits

| Limit | Value | Source |
|---|---|---|
| Inbox line/buffer cap | 1 048 576 bytes (1 MiB) | [OBS] bisected |
| Silent connection deadline | 30 s | [BIN] [DOC] |
| Sender-side message size cap | ~1 000 000 chars serialized, refused before sending | [DOC] |
| Accepted delivery queue | 50 messages | [DOC] |
| Held-message store | 100, oldest evicted | [DOC] |
| Idle subscription lifetime | 12 h | [DOC] |
| `dialogExpiry` default | 5 min (`"never"` supported) | [DOC] |
| `peerToken` | 128 bits, 32 hex chars | [OBS] |
| `msgV` | 1 | [OBS] |
| `peerProtocol` | 1 | [OBS] |

---

## 9. Conformance checklist

A peer that Claude Code sessions can discover and message must:

- [ ] Write `~/.claude/sessions/<pid>.json`, mode `0644`, compact JSON, with at least
      `pid`, `sessionId`, `cwd`, `startedAt`, `procStart`, `version`, `peerProtocol`,
      `peerFeatures`, `kind`, `entrypoint`, `pidDomain`, `messagingSocketPath`, `name`,
      `nameSource`, `status`.
- [ ] Derive `procStart` from `ps -o lstart=` in **UTC**, byte-exact.
- [ ] Write `~/.claude/sessions/<pid>.<sha256(socketPath)>.key`, mode `0600`, containing a
      32-hex `peerToken`, the same `procStart`, and `pidDomain`.
- [ ] Bind `<dir>/cc-socks/<pid>.sock`, chmod `0600`, in an accepted directory.
- [ ] Keep `status` and `updatedAt` current.
- [ ] Remove all three artifacts on exit, and handle SIGTERM/SIGINT.
- [ ] Accept and silently discard zero-byte probe connections.
- [ ] Parse newline-delimited JSON; drop the connection past 1 MiB buffered; close a
      connection silent for 30 s.
- [ ] Tolerate an absent or wrong auth line on macOS/Linux; require it on Windows.
- [ ] Ignore unknown `type` and unknown control `action` without erroring.
- [ ] Reply by connecting to the inbound `from` path.
- [ ] Emit `peer_message_status` on drop, with `orig_msg_id`, `drop_reason`, and coalesced
      `dropped_msg_ids`.
- [ ] Never treat `from` / `from-name` as authenticated; key identity on the kernel peer pid.

A client delivering into a real session must additionally:

- [ ] Vet the endpoint before sending: not a symlink, owned by this user, holding the expected
      pid, with a matching process start; abort with nothing sent when a check fails.
- [ ] Read the **target's** key file for the auth token.
- [ ] Generate a fresh UUIDv4 `msg_id` per message and retain it to correlate status frames.
- [ ] Wrap the body in a `<cross-session-message>` envelope with `from`, `from-name`, and the
      configured sender-declared `from-mode` class.
- [ ] Vary message content or pace sends; identical repeats in a short window are dropped.
- [ ] Refuse locally past the ~1 000 000-character serialized cap.

---

## 10. Known unknowns

Things a conforming implementation should verify itself rather than trust here:

1. **`peer_idle_notice` exact field set.** Not captured. Two subscription attempts produced
   only probe connections with no payload, so the notice was never emitted to the fake peer —
   most likely the sender's endpoint vetting rejected it. The frame name and `orig_msg_id`
   correlation are from the binary; field names beyond that are unverified.
2. **Which token the own-child path accepts on macOS.** Because the auth line is optional
   there, a valid, an invalid, and a missing token are externally indistinguishable. The
   `CLAUDE_CODE_MESSAGING_TOKEN` / `peerToken` split is confirmed as real (the values differ),
   but their individual acceptance was not isolated.
3. **Windows named pipes.** Entirely untested here.
4. **`bridge:` and `did:` reply schemes.** Present in the address regex; only `uds:` observed.
5. **Probe-connection count.** Two per send, consistently, in this version. Do not depend on it.
6. **Held/refused status frames.** Only `dropped` was captured, because the test sessions
   ran with `crossSessionInbound: accept`. The other statuses come from binary strings.

---

## Reproducing

From any directory, run the included live test against a peer the user has
explicitly authorized:

```sh
python3 /path/to/checkout/scripts/live_smoke.py --target example-peer --report /tmp/bridge-live-report.json
```

The test creates a temporary peer, requests a single acknowledgment, and removes
its own discovery, socket, and key artifacts on exit. Reports can contain private
local peer metadata and should remain outside the repository. Unit tests use
isolated temporary registries and do not contact existing sessions.
