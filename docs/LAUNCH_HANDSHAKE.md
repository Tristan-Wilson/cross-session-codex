# Launch handoff protocol, version 1

This optional integration lets a local operator's launcher prepare another
service for an exact Codex conversation before opening its UI. It does not add
Telegram, MCP, profile management, a shell hook, or a second session supervisor
to Cross Session Codex. Ordinary `launch` remains unchanged.

## Capability discovery

`cross-session-codex capabilities` is a side-effect-free JSON command:

```json
{"name":"cross-session-codex","version":"0.1.3","cli_api":1,"features":["launch-handshake-v1"]}
```

Consumers must check the executable name, `cli_api`, and required feature rather
than infer compatibility from a development version. They may ignore additional
capability fields and features. Installers should pin the companion artifact
separately. There is no automatic download or upgrade in this protocol.

## File descriptors and ownership

The parent creates two anonymous pipes and starts:

```text
cross-session-codex launch [normal launch options] --ready-fd N --continue-fd M
```

`N` is the child's write end of the ready pipe. `M` is the child's read end of
the continue pipe. Both flags are required together, descriptors must be distinct
and at least 3, and they must be pipes open in the indicated direction. They are
owned by the child once accepted and marked close-on-exec. They must not be
stdin, stdout, or stderr. Normal streams remain attached to the terminal.

Cross Session Codex remains one live process throughout preparation and the
handoff. Its PID and process start identity own the local registration. On
continuation it replaces itself with the Codex UI using `exec`, preserving that
PID. There is no prepare-and-exit registration and no later attach-by-name.

The launcher holds its existing app-server launch/shutdown lock while waiting.
Other launches and guarded shutdown cannot interleave with this handoff.
Consumers must not call another launch or shutdown while handling readiness.
An independent app-server client can validate the thread without this lock.

## Ready frame

After exact thread creation/resume, bootstrap if new, and local registration,
the child emits one newline-terminated JSON object, at most 8192 bytes including
the newline:

```json
{"v":1,"event":"ready","nonce":"random-per-launch","thread_id":"exact-uuid","codex_home":"/absolute/home","app_server_socket":"/absolute/socket","owner_pid":1234,"owner_start":"process-start-identity","name":"desk-gateway","version":"0.1.3"}
```

Paths identify the selected environment; no credentials are included. The
socket path may be a supported Codex symlink. `owner_start` is opaque. `nonce`
is 32 lowercase hexadecimal characters, unpredictable and unique to this
handshake; it is not a network credential. `thread_id` is a canonical lowercase
UUID. Paths are made absolute and cleaned, but symlinks are not resolved.
`ready` means the exact thread and local peer are prepared, not that the UI has
attached or that any requested work has completed.

For example, an optional Botbus controller can validate `codex_home` against an
explicit preconfigured profile and pin that identity to `thread_id` through its
operator API. It must not infer a thread from the local display name, inspect
the newest conversation, rewrite binding files, or rely on UI-only flags to
configure an already-running app-server. Root-follow is not appropriate for a
profile containing multiple unrelated sessions.

## Continue or cancel

The parent sends exactly one newline-terminated JSON frame on the continue
pipe, then closes its write end:

```json
{"v":1,"nonce":"same-random-per-launch","action":"continue"}
```

Alternatively, `action` is `cancel`. Version, nonce, and action are required;
unknown fields, unsupported versions/actions, a mismatching nonce, trailing
bytes after the newline, missing newline, and frames over 8192 bytes are errors.
The child requires EOF after the single frame, so the parent must close the
write end. EOF without a frame means cancellation (for example, parent exit).

The entire exchange, including writing readiness and reading through EOF, has
a fixed 30-second deadline after local registration. Timeout and explicit
cancellation are reported as launch failures. SIGINT, SIGTERM, and SIGHUP also
cancel an in-progress handoff. File descriptors close before the UI executes.

## Failure and external side effects

If the handoff fails or is canceled, the child unregisters its local peer and
retains the durable conversation and inbox history. It does not stop the shared
app-server or affect other sessions. A newly bootstrapped but canceled thread
therefore remains in history without a live peer. Failures before `ready` also
close the pipes and return a nonzero exit status.

The parent must supervise the child's exit status. EOF on the ready pipe is
**not** proof of successful `exec`: both successful exec and a failing process
can close descriptors. This protocol provides no separate UI-attached receipt.

The handoff is not a distributed transaction. If the controller activates an
external service before continuing, that service may already deliver to the
loaded thread. A subsequent cancellation cannot undo accepted messages or
external actions. Controllers must report such partial success accurately and
use their own guarded recovery mechanisms; Cross Session Codex never rewrites
another service's state. Messages admitted before UI attachment remain part of
the exact conversation, subject to that service's delivery guarantees.

## Scope and trust

This is a local operator-selected integration. A peer message cannot authorize
starting a session, selecting a Telegram identity, or expanding tool access.
The handshake does not make an arbitrary Codex profile local-only: its existing
plugins and MCP configuration still apply. Use an explicitly prepared profile
when isolation is required. Local-only standalone installations need no Botbus.
