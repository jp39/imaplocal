# imaplocal

imaplocal is an experimental IMAP/Maildir daemon that combines synchronization,
IMAP IDLE monitoring, and incoming Sieve filtering. Mail clients continue to
read and modify ordinary Maildir files. SMTP submission and outgoing queueing
remain separate.

**Status: experimental alpha, tested against isolated TLS IMAP and an external
provider, with initial use on a large mailbox.** It is not a drop-in replacement
for an existing mbsync installation: existing sync state cannot be adopted.
Extended reliability testing is ongoing. Nothing in this project installs a
service or modifies another email stack automatically.

## Build and test

Go 1.26 or later is required. Native macOS filesystem notifications require
cgo; builds without it use periodic reconciliation instead. Linux builds use
inotify through fsnotify. SQLite does not require cgo.

```sh
go build -o bin/imaplocal ./cmd/imaplocal
go test -race ./...
go vet ./...
```

The default tests use temporary directories, synthetic messages, a fake remote
for fault injection, and an in-process IMAP server on loopback with a temporary
trusted TLS certificate. They never read real mailbox credentials or contact
Office 365.

## First run

Copy [the example configuration](examples/config.json) to a private location
and adjust its host, user, credential command, and paths. Relative paths are
resolved against the configuration file's directory. Unknown configuration
keys are errors. `~` expansion is supported for filesystem paths, not executable
arguments.

The Maildir must be new and empty on first use. Sync metadata and staged
messages live in its private `.imaplocal/` subdirectory, created with mode 0700.
Only `maildir` is configured; there is no separate `state_dir` setting.
An ownership marker binds the Maildir to the state database and account identity.
Existing Maildirs, including mbsync-managed ones, are rejected.

```sh
./bin/imaplocal check -config /path/to/config.json
./bin/imaplocal sync -config /path/to/config.json
./bin/imaplocal status -config /path/to/config.json
./bin/imaplocal run -config /path/to/config.json
```

`check` compiles the Sieve script and validates configuration without running a
credential command, making a connection, or initializing state. `sync` runs
one cycle and exits unsuccessfully if it encounters an error or unresolved
issues. `run` uses native notifications, optional IDLE connections, and periodic
reconciliation, with reconnection backoff. Failed cycles retry after 1, 2, 4,
8, 16, 32, then at most 60 seconds; notifications cannot shorten that delay.
A successful cycle resets the backoff. IDLE workers retry independently and
request catch-up after reconnecting, rather than on each failure.
SIGINT/SIGTERM stop its workers, including during a retry wait.
`status` reads the persisted health, membership counts, issues, and unfinished
operations while another process may be running.

Start with `remote_writes: false` and `filter_folders: []`, as in the example.
This permits remote inventory/download and remote-to-local flag updates.
Local changes requiring remote writes are retained and reported as issues.

To synchronize every selectable remote folder, use:

```json
"folders": ["*"]
```

Patterns use IMAP wildcards: `*` matches any number of characters, including
`/`, and `%` matches within one hierarchy level. For example, `INBOX/*` selects
all descendants of INBOX, while `INBOX/%` selects direct children. Neither
includes INBOX itself; list `INBOX` separately. Other characters are literal.
You can combine patterns and exact names; overlaps synchronize a folder once.

The standalone `*` includes subscribed and unsubscribed folders at every level.
Matching remote folders are rediscovered on every reconciliation, including
while `run` is active. Patterns never create literal wildcard folder names.
Empty mailboxes also get local `new`, `cur`, and `tmp` directories.
`filter_folders` still requires literal names covered by the folder selectors,
so selecting folders does not enable filtering everywhere.

For a mail-only account selection with an INBOX hierarchy:

```json
"folders": ["Archive", "Deleted Items", "Drafts", "INBOX", "INBOX/*", "Junk Email", "Sent Items"]
```

Narrowing selectors pauses previously tracked folders outside the selection,
including folder rename/removal propagation, without deleting their local mail
or saved mappings. Reselecting resumes them. Already-journaled operations and
arrivals are allowed to finish; local transfers from selected messages and Sieve
destinations still receive the inventories needed to complete their work.
Paused folders can remain visible in a Maildir client.

Folder names are preserved (`/` becomes local directory nesting). Nonselectable
containers are skipped; names that cannot be represented safely cause an error
rather than being silently ignored. Folder rename/removal behavior is described
below. Previously unprocessed messages in `filter_folders` are filtered even
on the first inventory of a newly discovered folder.

IDLE uses up to eight connections, with INBOX prioritized. All other selected
folders still synchronize on the periodic timer (`interval`, default 30s).
`run` starts with an immediate reconciliation; a preceding `sync` is optional.

To enable filtering and bidirectional changes in a **dedicated test account**,
set `remote_writes: true` and `filter_folders: ["INBOX"]`. Copy/move/upload
operations require UIDPLUS; moves additionally require MOVE. There is no
fallback that expunges unrelated messages.

Each previously unprocessed remote membership in `filter_folders` receives a
recorded Sieve delivery plan, including messages present at first inventory.
Completed messages are not automatically filtered again after restarts, flag
changes, folder renames, or rule edits. Processing state lives in SQLite;
message headers and raw bytes are never rewritten to record it.

Existing completed imports from older versions remain completed. To reprocess
those or apply edited rules, use the explicit `filter` command.

Configuration and rules are loaded at startup. Restart after editing them.
Do not remove the state database to force reprocessing.

## Command reference

Every command requires `-config FILE`. Configuration and Sieve rules are loaded
at startup; restart the daemon after changing them.

| Command | Behavior |
| --- | --- |
| `check` | Validate configuration and Sieve without connecting or creating state. |
| `sync` | Reconcile once and exit; errors or unresolved issues produce a nonzero exit code. |
| `run` | Reconcile immediately, then continue using IDLE, filesystem events, and periodic scans. Stop with Ctrl-C or SIGTERM. |
| `status` | Show readable health, folder progress, connection state, issues, and unfinished operations; works while the daemon runs. |
| `filter` | Preview current rules against completed local mail; use `-apply` to reprocess explicitly. |
| `resolve` | Record a verified outcome for an uncertain message or folder operation; stop the daemon first. |

```sh
./bin/imaplocal help
./bin/imaplocal -version
./bin/imaplocal resolve -h
```

`filter` defaults to an offline dry run; it does not invoke credentials or
change mail/state and can run while the daemon is running. It uses
`filter_folders` by default. Repeat `-folder` for literal source folders or
`-message-id` to select exact Message-IDs (all matching copies are selected).
A Message-ID is a selector here, never a deduplication identity.

Limit selection by received time with `-since 2026-10-01` (inclusive local
midnight), or an RFC3339 timestamp such as `-since 2026-10-01T00:00:00+02:00`.
Alternatively, `-within 10d` means the last 240 hours; `-within 24h` also works.
The options are mutually exclusive and combine with folder/Message-ID selection.
They use the server's IMAP INTERNALDATE, independent of the sender's Date header
and filesystem modification times. Relative cutoffs are resolved once when the
command starts and printed in preview/apply output. Each proposed change also
shows its received timestamp in local time with a timezone offset (`received=`). Existing arrival journals
supply dates for older entries; an unavailable timestamp is reported as skipped
rather than guessed. The updated daemon records received dates during sync.


```sh
./bin/imaplocal filter -config account.json
./bin/imaplocal filter -config account.json -folder INBOX -within 10d
./bin/imaplocal filter -config account.json -folder INBOX -since 2026-10-01
./bin/imaplocal filter -config account.json -folder INBOX -message-id '<example@host>'
# Stop the daemon before applying the previewed selection:
./bin/imaplocal filter -config account.json -folder INBOX -message-id '<example@host>' -apply
```

Applying requires `remote_writes: true`, the exclusive writer lock, and completed
pending operations. Synchronize local edits first. Apply validates current local
bytes/flags and remote membership/flags before journaling each plan. Interrupted
plans resume with normal `sync`/`run`. Uncertain native moves retry when the
original source UID still exists in the same UIDVALIDITY; other uncertain
results require `resolve`. Repeating an explicit copy rule may create additional legitimate
copies. Preview skips unchanged keep decisions; it does not mark them processed.
Header parsing ignores malformed fields and their continuations, while retaining
valid fields before and after them. This tolerant view is shared by normal
arrival filtering, manual filtering and Message-ID diagnostics; stored and
transferred message bytes remain unchanged. Logs include `ignored_headers`
when fields were rejected. Per-message Sieve evaluation failures are reported
with filenames and UID context, counted as skipped, and left untouched; other
messages continue through preview/apply. Filesystem, state and synchronization
conflicts still abort the command.

Runtime logs report received messages, filter start/results, remote operations,
Maildir publication and flag changes. Message events include Message-ID when
available and folder/UIDVALIDITY/UID; local events include filenames. Filesystem
events report paths and operations, including the daemon's own writes, and are
hints rather than proof of new mail. Private `.imaplocal` activity is excluded.
Bodies and credential-command output are not logged. A concurrent MUA rename
invalidates the observation and schedules a rescan without reconnecting IMAP;
connection failures report their reconnect delay. Progress summaries remain.

`status` derives counts using SQLite summary indexes and loads only pending
operations, rather than decoding every completed message/arrival record. The
indexes are created automatically on the next writable open; read-only status
also supports older databases without modifying them. No cached synchronization
counters or separate ledger are maintained.

`status` prints plain text; there is no JSON output mode. It shows:

- Read-only or bidirectional mode from the supplied configuration.
- The last recorded daemon state, completed cycle, error, and scheduled attempt.
- Per-folder remote inventory counts, tracked memberships, inventory timestamps,
  and last-known IDLE connection/retry state. Other folders use periodic scans.
- Grouped issue explanations and pending operation IDs needed by `resolve`.

For example, a local read change with remote writes disabled is reported as
`1 local flag change blocked: remote writes disabled`. Warning logs include the
same explanation alongside the counts.

These are persisted observations, not a liveness probe: a crash can leave an old
state behind, and the displayed update time matters. Inventory completion is
separate from message reconciliation; mapped membership counts grow as messages
are imported. Older stores display unknown observations until the updated daemon
runs. Status never executes credential commands or connects to IMAP.

For an uncertain operation, use the ID reported by `status` and either a verified
remote destination mapping:

```sh
./bin/imaplocal resolve -config /path/to/config.json \
  -id OPERATION_ID -uid DESTINATION_UID -uidvalidity DESTINATION_UIDVALIDITY
```

or, only after verifying that no remote effect occurred:

```sh
./bin/imaplocal resolve -config /path/to/config.json \
  -id OPERATION_ID -not-applied
```

The next reconciliation validates a supplied mapping against the staged body.
`-not-applied` authorizes retry; guessing an outcome can duplicate mail. See the
recovery section below for the operation journal and resolution behavior.

## Authentication and TLS

All IMAP connections use implicit TLS with certificate and hostname
verification. Set `ca_file` only when a private/test CA is needed; there is no
skip-verification switch or plaintext mode.

Set `proxy_url` to an HTTP CONNECT proxy, for example
`"http://proxy.example.org:80"`, when direct IMAPS is unavailable. HTTPS proxy
URLs are also supported. IMAP TLS still verifies the destination hostname;
HTTPS proxies additionally require a trusted proxy certificate. The connection
timeout covers connecting, CONNECT negotiation, and TLS. Proxy authentication
and SOCKS are not implemented; environment proxy variables are not used.
Omit `proxy_url` for a direct connection.

`auth` accepts `xoauth2` (default) or `plain`. The latter uses IMAP LOGIN over
the verified TLS connection. `credential_command` is an argument array executed
directly, without a shell. It must output exactly one token/password line.
For example, an existing OAuth token helper can be used as a one-element
command array. A protected password file can be read by an explicit local
command such as `["/bin/cat", "/private/path/password"]`.

Credential commands are serialized in the process, bounded in output and
execution time, and their stderr is not included in errors. No token store is
imported, no raw protocol debugging is enabled, and credentials are not placed
in the SQLite journal. Separate processes using the same OAuth helper should
retain that helper's own cross-process refresh lock.

## What is implemented

- Batched complete UID/flag/date/size inventory for configured mailboxes and
  known local/filter destinations; only new memberships need body downloads.
- Streaming body batches select once per group of up to 64 UIDs from the same
  mailbox/UIDVALIDITY, with a 16 MiB target based on inventory sizes. A larger
  allowed message is fetched alone. Actual literal sizes still enforce the
  configured per-message limit. Whole batches are never retained in memory.
- Staged body downloads with a configurable size limit and preservation of raw
  message bytes.
- Sieve header-based filing, copies, implicit/explicit keep, stop, and portable
  imap4flags, using go-sieve. Unsupported extensions/actions fail closed.
- Recursive nested destination creation and server-side COPY/MOVE with returned
  UID mappings.
- Atomic Maildir publication through `tmp/`, with deterministic application-owned
  keys. As in mbsync, Seen imports go into `cur/` and unread imports into `new/`;
  applying a later flag update acknowledges the message into `cur/`.
- Three-way merging of independent portable flag changes; updates use flag
  deltas so unknown remote keywords are preserved.
- Uploading externally added `cur/` messages and detecting moves/copies that
  retain their managed key. Copies get distinct local/remote identities.
- Durable operation intent, crash recovery, operator resolution for uncertain
  transfers, and bidirectional message deletion with conflict preservation.
- One writer per state directory, read-only status access, account/root binding,
  dropped-event reconciliation, periodic catch-up, and reconnect backoff.
- Bounded IDLE connections for selected/discovered folders. Other
  discovered destinations are still covered by periodic inventory.

Maildir placement deliberately follows mbsync rather than the original
new-delivery-only protocol: a fully written, synced Seen import is published
atomically into `cur/`. This prevents already-read mail appearing as new and
protects its flags from readers that reset suffixes when acknowledging `new/`.
Unread imports retain their flags in a `:2,` suffix in `new/`. aerc's current
Maildir library discards that suffix during acknowledgement, so **unread imports
with non-Seen flags can still lose those flags locally**. This compatibility
limit is retained to match mbsync; aerc is not patched for it. Clearing Seen on
an acknowledged message keeps it in `cur/`, so unread does not mean new.

Portable flags are Seen, Answered, Flagged, Draft, and `$Forwarded`, represented
as Maildir S/R/F/D/P. Custom IMAP keywords are preserved on the server but are
not represented in Maildir. Maildir T requests deletion of a tracked
membership; it is handled separately from portable flags. Other unknown flag letters stop that scan rather than
being silently discarded. Unmapped local T messages are never uploaded.

## Folder renames and removals

Folder identities are recorded on the first successful observation. Each local
Maildir gets a hidden `.imaplocal-folder` marker so directory renames, including
empty folders, can be recognized. Existing alpha state is adopted automatically
from its saved boundaries. If an older store already has a missing local folder
while it still exists remotely, restore the directory before using this feature;
the upgrade will not infer destructive intent without an established marker. Preserve this marker when renaming a folder;
copying a whole directory with its marker is a conflict, not a folder rename.

With `remote_writes: true`, a local directory rename becomes IMAP RENAME.
Message identities and completed filtering records follow the renamed
subtree; messages are not uploaded again. Literal `folders`, `filter_folders`,
and Sieve destinations follow recorded local renames while those aliases remain
unambiguous. INBOX renames/removals and occupied destinations are rejected.
Namespace renames currently require the server's `/` hierarchy layout.

Local folder removal becomes IMAP DELETE, processing children before parents.
**No archive or recovery copy is created; deletion is permanent.** Servers
that refuse deletion of populated mailboxes produce an error. imaplocal does
not force an EXPUNGE fallback. With remote writes disabled, local renames and
removals remain blocked and are reported through `status`.

A successful complete remote LIST which omits a previously tracked folder
removes its unchanged local mailbox contents and retires its mappings, even
in read-only mode. Surviving child folders are preserved. Unmapped drafts,
modified bodies, pending local flag changes, and in-progress deliveries block
cleanup. Failed LIST/SELECT calls are never treated as evidence of deletion.

The current IMAP adapter has no stable mailbox-ID support. A rename performed
by another IMAP client therefore appears as removal plus a new folder import;
imaplocal does not guess identity from UIDVALIDITY, Message-ID, or body hashes.
Use `folders: ["*"]` to discover that new name. Pending local edits in the old
folder must be resolved before cleanup. Individual-message deletion follows
the separate policy below.

Folder operations are journaled, and namespace metadata changes commit in one
SQLite transaction. An interrupted RENAME/DELETE with an unknown outcome is
not retried. Inspect pending folder operations in `status`, stop the daemon, and record
an independently verified outcome with one of:

```sh
./bin/imaplocal resolve -config /path/to/config.json \
  -folder -id OPERATION_ID -applied
./bin/imaplocal resolve -config /path/to/config.json \
  -folder -id OPERATION_ID -not-applied
```

The next sync validates the resulting namespace. For an applied rename it also
validates destination epochs and tracked message bytes before rebinding them.
`-not-applied` permits a retry and must only be used after checking the server.
There is no archive restoration command.

## Message deletion and moves

Removing a tracked Maildir file (as aerc does when deleting a message), or
adding its `T` flag, requests permanent deletion of that remote membership.
`remote_writes: true` is required. The intent is journaled, then confirmed against a fresh complete local scan
before the remote request in that same cycle. One `sync` is sufficient; `run`
applies it when it reconciles the change. A file that reappears or moves before
the request is sent cancels the old deletion intent.
Incomplete Maildirs, failed scans, and UIDVALIDITY changes cannot authorize it.

Deletion requires UIDPLUS and uses `UID STORE +FLAGS.SILENT (\Deleted)` followed
by `UID EXPUNGE` for that single UID. No mailbox-wide EXPUNGE or CLOSE fallback
is used, so unrelated messages marked deleted by other clients remain intact.
A lost acknowledgement is recovered by inventory: an absent UID completes the
operation; a present UID can be retried in the same epoch after checking intent
and flags again. Unlike uncertain APPEND/COPY or a MOVE with an absent source,
deletion does not require an operator-supplied destination UID or `resolve`.

A remote disappearance removes an unchanged local home copy, even in read-only
mode. Local body edits, flag changes, copies, or moves conflicting with that
removal retain local content and report `delete_conflict`. A file restored after
an uncertain deletion is also retained; explicitly mark it T or remove it to
accept that deletion. Tombstones prevent
re-uploading vanished memberships. This also applies to previously retained
tombstoned copies when upgrading. No archive or recovery copy is created.
Deleting a message is permanent; moving it to Trash is an ordinary move instead.

Managed-key local moves already use server-side IMAP MOVE, requiring MOVE and
UIDPLUS. Local copies use COPY and receive a distinct managed identity. A move
made by another IMAP client appears as an absent source plus a newly imported
destination; unchanged old local copies are removed without guessing identities
from Message-ID or matching bodies. Select both folders (or use `folders: ["*"]`)
to observe both sides. Moves that rewrite the managed filename cannot be
correlated and appear as separate deletion and upload intents.

Conflict detection uses observed flags and verified local bytes, not an atomic
transaction with another client. A client can still change flags or restore a
file after the last check; CONDSTORE-based conflict protection is future work.

## Recovery and uncertain outcomes

SQLite records intent before an APPEND, COPY, or MOVE is sent. If a response is
lost or a process stops while a request may be in flight, the operation becomes
`unknown`. Before retrying an uncertain native MOVE, imaplocal checks that the
original source UID still exists under the same UIDVALIDITY. A successful
check permits retry through the normal journal and capability checks, and logs
`retrying uncertain MOVE; original source still exists`. This also applies to
operations recovered after a restart. A failed lookup, changed epoch, or absent
source leaves the operation paused. An absent source does not establish which
destination UID resulted from the MOVE; Message-ID and body matches are not
used to guess that mapping. Concurrent changes after the check can still leave
the retry uncertain, in which case the same recovery policy applies next time.

Uncertain APPEND and COPY operations are not retried automatically. Discovery
of previously unmapped remote messages in an uncertain operation's destination
is also paused, preventing its effect from being imported as a second message.

Use `status` to inspect the operation. After independently identifying its
actual destination membership, stop the daemon and record its UID mapping:

```sh
./bin/imaplocal resolve -config /path/to/config.json \
  -id OPERATION_ID -uid VERIFIED_UID -uidvalidity VERIFIED_UIDVALIDITY
./bin/imaplocal sync -config /path/to/config.json
```

The next cycle fetches that membership and verifies its body hash before
binding it. A mistaken unverified mapping can be corrected with another
`resolve` invocation.

If an operator has established that **no remote effect occurred**, an explicit
`resolve ... -id OPERATION_ID -not-applied` allows a retry. This is an operator
assertion, not an automatic deduction. Do not use it merely because a message
was not immediately visible in another client.

Publication is recovered by the stable Maildir key, including acknowledgement
by the MUA. Completed scratch bodies are reclaimed only after durable completion;
interrupted plans keep their staged content. Never manually delete staging or
the operation journal to clear an error. Back up state and Maildir together:
the database holds essential UID and operation history, not a disposable index.

## Current boundaries

- One account per process/configuration. No launchd/systemd installation yet.
- Message deletion is permanent and requires UIDPLUS for remote writes.
  Only targeted UID EXPUNGE is used; there is no archive or undo workflow.
- UIDVALIDITY changes and body edits require review. Confirmed folder removals
  retire their mappings; config and Sieve rules cannot silently recreate them.
- Incoming remote moves are observed as a vanished source and new destination;
  no Message-ID/hash heuristic merges their identities. Conflicting local
  changes retain their content for operator review.
- Local moves/copies can be correlated only while their `imaplocal-<id>` key survives.
  An unrelated filename is a new local entry, not proof of a move.
- Persistent I/O errors invalidate an observation; absence is never inferred
  from a failed scan. Filesystem symlinks are rejected in the managed tree.
- Sieve discard, redirect, envelope tests, and arbitrary keywords are not
  supported. A blocked filter leaves the
  original on the server and reports the failure rather than losing it.
- Full metadata reconciliation is the current fallback. CONDSTORE/QRESYNC,
  body streaming beyond the configured in-memory size
  bound, and large-mailbox performance tuning are still future work.
- There is no mbsync state importer or automatic production migration.

These boundaries are deliberate limits of the first implementation, not
claims of feature parity with mbsync or general IMAP server compatibility.

## Real-provider tests

An opt-in provider test is available in `internal/imap/provider_test.go`.
It is skipped unless a configuration path is supplied:

```sh
IMAPLOCAL_PROVIDER_CONFIG=/private/path/test-account.json \
  go test ./internal/imap -run '^TestProvider$' -v -timeout 5m
```

By default it only connects and inventories the configured folders. For an
explicitly authorized dedicated account, set `IMAPLOCAL_PROVIDER_WRITE_PREFIX`
to a unique new test-folder prefix. The test creates Source/Copy/Move folders
under it and appends/copies/moves synthetic messages. It also verifies an
IDLE arrival notification using a second connection. It does not submit SMTP
mail or expunge messages. Test folders/messages are intentionally retained for
inspection; remove them manually afterward if desired. Never point this at
production as an incidental part of development.

The reconciliation test additionally exercises initial-inventory filtering, explicit reprocessing, Sieve,
bidirectional flags, local moves/copies/uploads, local and remote message
deletion, remote moves, durable-state reopen, and
folder rename/removal in newly created synthetic folders:

```sh
IMAPLOCAL_PROVIDER_CONFIG=/private/path/test-account.json \
IMAPLOCAL_PROVIDER_WRITE_PREFIX=imaplocal-test-UNIQUE \
  go test ./internal/reconcile -run '^TestProviderReconciliation$' -v -timeout 5m
```

It creates a random suffix beneath the supplied remote prefix and retains a
private local test tree beside the supplied config. Its log gives both paths.
Those artifacts contain synthetic mail and account configuration; keep them
private. Real-provider tests supplement isolated crash/fault tests, and do not
establish production readiness or compatibility with other providers.

Provider results and remaining coverage limits are recorded in
[docs/provider-validation.md](docs/provider-validation.md).

## Source structure

```text
cmd/imaplocal/          CLI commands and signal handling
internal/daemon/      Scheduling, reconnects, IDLE workers, lifecycle
internal/reconcile/   Reconciliation, delivery plans, operation recovery
internal/model/       Distinct identities, portable flags, remote adapter contract
internal/state/       Locked SQLite record store and schema checks
internal/imap/        Verified TLS, authentication, inventory and remote operations
internal/maildir/     Owned storage, atomic publication, scans, native observation
internal/sieve/       Rule compilation and side-effect-free delivery decisions
internal/auth/        Bounded, serialized credential command
internal/status/      Persisted health and unfinished-work report
internal/testutil/    Isolated TLS IMAP server for tests
docs/design.md        Design rationale and future acceptance cases
```

imaplocal is licensed under the [MIT License](LICENSE).

Dependencies retain their upstream licenses: go-imap and go-sasl (MIT),
go-sieve (MIT), fsnotify/FSEvents (BSD), and modernc SQLite and its dependencies
(see their module license files and third-party notices). Preserve applicable
dependency copyright notices and license texts when redistributing binaries.

Background: [the original email stack](https://jp39.github.io/email-stack/),
[disconnected IMAP guidance](https://www.rfc-editor.org/rfc/rfc4549.html),
[UIDPLUS](https://www.rfc-editor.org/rfc/rfc4315.html), and
[CONDSTORE/QRESYNC](https://www.rfc-editor.org/rfc/rfc7162.html).

## Store layout

Each Maildir root contains `.imaplocal/owner`, `.imaplocal/state.sqlite`,
`.imaplocal/daemon.lock`, and private staging/quarantine directories. Each
tracked mailbox has an `.imaplocal-folder` marker that travels with folder
renames. Managed message filenames use `imaplocal-<id>:2,<flags>`.
The owner ID and folder/message IDs must agree with the database. Do not
remove or recreate them to repair synchronization.

## Upgrading a store with separate state

Stop the daemon and any commands that write its state. Move the **entire** old
state directory beneath the existing Maildir; do not copy only `state.sqlite`:
SQLite sidecars and staged messages belong with it. The destination must not
already exist. For example, with both paths under the current directory:

```sh
# First verify that maildir/.imaplocal does not already exist.
mv old-state maildir/.imaplocal
```

Remove `state_dir` from the JSON configuration, leaving `maildir` unchanged.
Run `imaplocal check` and `imaplocal status` with that configuration before
starting `imaplocal run`. Old configurations containing `state_dir` are rejected
rather than silently opening a new database. Existing message and folder
identities are retained, and no initial download or re-filtering is needed.
Do not run an older binary against a migrated store. The existing canonical
Maildir root binding still applies; this change does not implement relocating
the Maildir itself. To back up the store, stop the daemon and copy the entire
Maildir, including its hidden files and `.imaplocal/` directory.

## Unavailable message bodies

A message can disappear between inventory and body retrieval. A successful
body fetch without a body is checked again using UID metadata. Confirmed
absence is skipped for that observation. If the UID still exists (for example,
an Exchange calendar item that cannot currently be exported), status records a
`body_unavailable` issue and the remaining messages continue. The body is
retried on later cycles; no empty replacement is saved and no remote item is
deleted. Network/protocol failures still interrupt the cycle for reconnection.
Fetch errors include folder, UIDVALIDITY and UID. Wildcard selection includes
selectable calendar and other non-mail folders exposed by the server.
Inventory and message processing log progress about every five seconds.

## Batched body downloads

New bodies are requested with one UID FETCH per batch, using BODY.PEEK so
reading a message does not mark it read. Batches stay within one mailbox and
UIDVALIDITY epoch. Complete bodies are associated with their returned UID,
staged and journaled individually while streaming. Truncated literals are
rejected using their declared byte length. Copies, moves and flag changes
from Sieve execute only after the fetch has fully drained.

If the connection fails halfway through a batch, completed body checkpoints
survive. The next cycle recovers those plans and fetches only the remaining
memberships. Existing stores and configurations need no migration or reset.
The daemon still refreshes complete metadata inventories after reconnecting;
this change reduces body-fetch round trips, not inventory work. No throughput
claim is made until measuring the real-provider import.

If a fetched MIME literal exceeds `max_message_bytes` even though its advertised
RFC822.SIZE was smaller, the actual size is checkpointed before closing the
connection. That first observation may require a reconnect without draining the
large body. Later cycles skip that UID with a `message_too_large` issue and keep
syncing the remaining mail. Increasing the limit above its observed size makes
it eligible again. No empty replacement is published and no remote message is
deleted. The default limit is 64 MiB; accounts with larger mail need a larger
explicit value.
