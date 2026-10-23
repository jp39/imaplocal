# Commands and diagnostics

[README](../README.md) · [Configuration](configuration.md) · [Recovery](operations.md#recovery-and-uncertain-outcomes)

Every command supports an optional `-config FILE` override. Without it,
imaplocal uses [default config discovery](configuration.md#configuration-discovery).
Configuration and Sieve rules are loaded at startup; restart the daemon after
changing them.

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

## Running the daemon

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

IDLE uses up to eight connections, with INBOX prioritized. All other selected
folders still synchronize on the periodic timer (`interval`, default 30s).
`run` starts with an immediate reconciliation; a preceding `sync` is optional.

All selected folders participate in periodic inventory, including those with
IDLE connections. Progress logs are throttled and may show only a subset of
folder names; omitted names do not mean those folders were skipped.

## Reprocessing filters

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
shows its received timestamp in local time with a timezone offset (`received=`).
Existing arrival journals supply dates for older entries; an unavailable
timestamp is reported as skipped rather than guessed. The updated daemon records received dates during sync.

```sh
./bin/imaplocal filter
./bin/imaplocal filter -folder INBOX -within 10d
./bin/imaplocal filter -folder INBOX -since 2026-10-01
./bin/imaplocal filter -folder INBOX -message-id '<example@host>'
# Stop the daemon before applying the previewed selection:
./bin/imaplocal filter -folder INBOX -message-id '<example@host>' -apply
```

Applying requires `remote_writes: true`, the exclusive writer lock, and completed
pending operations. Synchronize local edits first. Apply validates current local
bytes/flags and remote membership/flags before journaling each plan. Interrupted
plans resume with normal `sync`/`run`. Uncertain native moves retry when the
original source UID still exists in the same UIDVALIDITY; other uncertain
results require `resolve`. Repeating an explicit copy rule may create
additional legitimate copies. Preview skips unchanged keep decisions; it does
not mark them processed.
Header parsing ignores malformed fields and their continuations, while retaining
valid fields before and after them. This tolerant view is shared by normal
arrival filtering, manual filtering and Message-ID diagnostics; stored and
transferred message bytes remain unchanged. Logs include `ignored_headers`
when fields were rejected. Per-message Sieve evaluation failures are reported
with filenames and UID context, counted as skipped, and left untouched; other
messages continue through preview/apply. Filesystem, state and synchronization
conflicts still abort the command.

## Logs and status

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

## Resolving operations

Use the operation ID from `status` and stop the writer before recording an
outcome. See [message recovery](operations.md#recovery-and-uncertain-outcomes)
for verified UID mappings and `-not-applied`, or
[folder recovery](operations.md#folder-renames-and-removals) for `-folder`.
