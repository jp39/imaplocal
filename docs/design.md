# Design notes

This document records the design rationale and remaining implementation goals.
The README describes current executable behavior. The alpha now implements
staged delivery, Sieve plans, SQLite journaling, portable flags, uploads,
managed-key moves/copies, native observation, and IDLE. Deletion propagation,
state migration, CONDSTORE/QRESYNC, and broad provider coverage remain future
work. Standalone `*` selection discovers selectable remote folders each cycle;
IDLE watchers follow that selection with an eight-connection limit and INBOX
priority. Empty mailboxes are represented locally. Remote writes are opt-in and the existing production stack is untouched.

## Scope

Build a single daemon that monitors an IMAP account, synchronizes complete
messages and flags with a local Maildir, evaluates Sieve rules for incoming
mail, and propagates external Maildir changes back to the server.

The first target is an Office 365 account used with aerc, on macOS and Linux.
The implementation should not assume an extension is available simply because
it would be convenient. Inspect server capabilities and support a documented
fallback or explicitly reject an unsupported configuration.

SMTP submission, an MUA, a search index, and contact management are outside
the initial scope. Keep an external OAuth token-command interface available;
built-in authorization flows can be considered later.

## Ownership and boundaries

The reconciliation engine receives observations and constructs explicit plans.
Protocol and storage adapters execute plans and report outcomes. The durable
state layer records checkpoints and operation transitions. Scheduling, IDLE,
filesystem monitoring, and timers belong to the daemon layer.

Do not let adapters independently decide deletion or conflict policy. Do not
let callbacks mutate local and remote stores without a durable account of the
operation. Use account/mailbox ordering where required; avoid turning one slow
mailbox into a global lock on all future accounts.

## Identity model

Distinguish:

- message content and an application-owned logical identifier;
- a local Maildir entry and its current folder/filename;
- a remote mailbox membership, identified within a UIDVALIDITY epoch;
- a pending operation, with its own stable operation identifier.

A copy creates another membership, even when its content is identical.
Message-ID and content hashes are evidence, not universally unique message
identities. A remote UID is scoped to a mailbox and UIDVALIDITY. A filename,
including an mbsync `,U=` marker, is not a global identity.

Define folder rename and UIDVALIDITY-reset behavior before permitting automatic
repair that might recreate or delete mail. Do not reuse stale mappings.

## Incoming delivery and filtering

The implemented incoming pipeline is:

```text
remote observation → private download staging → Sieve decision
                   → recorded publication plan → visible Maildir delivery
                   → remote membership/flag reconciliation
```

Keep the database, lock, and private staging in `.imaplocal/` at the Maildir
root. Message/folder scans and filesystem notifications exclude this reserved
directory; staging never appears inside a mailbox's `new/`, `cur/`, or `tmp/`.
There is no independent state path in configuration. Publish complete
files using the mbsync-compatible placement policy: Seen imports are published
atomically from `tmp/` into `cur/`, while unread imports go into `new/` with a
flag suffix. Applying a subsequent flag update moves the message into `cur/`,
as mbsync does. Direct Seen import is a deliberate departure from the original
Maildir new-delivery protocol. Acknowledgement into `cur/` does not mean read.
Readers such as aerc may discard non-Seen flags on unread imports during
`new/` acknowledgement; matching mbsync retains this interoperability limit.

Record whether a remote membership has been handled for arrival filtering and
which rule revision/decision was used. Every previously unprocessed membership
in a filter folder is eligible, including the first inventory. Rule edits do
not automatically repeat completed decisions. Explicit `filter` previews and
optionally reapplies current rules to selected completed local memberships,
using the existing arrival/operation journal for recovery. Its original mapping
is reserved until replacement deliveries and cleanup finish. Keep processing
records in SQLite, never inject state headers into immutable raw messages.

For `fileinto`, `fileinto :copy`, keep, and flag changes, preserve the relation
between the original remote membership and intended destinations. A known
remote message filed into another mailbox should use a server-side operation
when supported and appropriate, rather than automatically uploading its body
again. Acknowledged mappings, retries, and destination publication all need
recovery semantics. Do not promise exactly-once effects across IMAP and disk.

Reuse an existing Sieve interpreter. Initially support only the subset actually
needed: header tests, fileinto, copy, keep, stop, and imap4flags. Unsupported
extensions and rule failures need an explicit non-destructive policy. Discard is currently rejected entirely; any future support must require
explicit configuration.

## Durable operations and recovery

The implementation uses SQLite through modernc.org/sqlite, with WAL and FULL
synchronous durability. Typed JSON records are grouped by kind and stable key
in a versioned database. One writer holds an OS lock; status uses read-only
access. No transaction is held across network operations. A more normalized
schema and incremental inventory are future performance work.

There is no atomic transaction spanning IMAP, a database, and Maildir. Model
operations so that intent is recorded before effects, and recovery can inspect
the result before continuing. Avoid holding a database transaction across a
network round trip.

An operation needs outcomes more precise than success/error: not attempted,
known rejected, known completed, and outcome unknown after disconnection are
different states. The exact state machine is an implementation task.

For example, an APPEND can succeed remotely even if its response is lost. A
retry must not blindly duplicate the message. UIDPLUS helps map acknowledged
results but does not resolve every ambiguous outcome. If an outcome cannot be
established safely, surface it rather than guessing destructively.

Local publication can likewise succeed before the journal records completion.
Recovery must recognize an already-published entry and must not rerun a copy
rule just because the process restarted.

## Conflicts and deletions

Keep the last agreed state as well as both current observations. Reconcile
independent changes and identify conflicting changes explicitly. Do not use
filesystem modification times as a universal last-writer-wins mechanism.

Decide policies for at least:

- local flag changes concurrent with remote flag changes;
- local move concurrent with a remote move or deletion;
- a remote deletion while a local change is waiting to upload;
- folder removal/rename and local state that was temporarily inaccessible;
- changed message bodies produced by an external program;
- unsupported flags and keywords.

Use tombstones or equivalent durable knowledge to prevent resurrection.
Distinguish absence from an incomplete scan or unavailable filesystem. Message
deletion now uses the confirmed-intent policy below. Conflicting local changes
are retained and reported; no deletion archive is created.

## Observation and performance

Treat filesystem events and IDLE as hints to refresh observations. Support
startup reconciliation, periodic checks, and recovery after dropped events.
Avoid feedback loops when the daemon observes its own writes, without relying
only on a time-based ignore window.

On macOS, use recursive FSEvents or another mechanism that avoids one descriptor
per message. Preserve a suitable Linux backend. Event loss must trigger bounded
reconciliation rather than historical refiltering.

IDLE requires connection lifecycle management, reconnect backoff, health
reporting, and reconciliation after gaps. Multiple selected mailboxes require
an explicit connection strategy. Use CONDSTORE/QRESYNC when actually supported,
with a correct fallback. Bound concurrency and batch metadata operations;
mailboxes of hundreds of thousands of messages must not require reading every
body on every check.

## Suggested implementation sequence

1. Define identifiers, observations, operation outcomes, and reconciliation
   policies. Test decisions with in-memory fake adapters and explicit history.
2. Implement durable state and recovery of planned local operations against
   temporary Maildirs. Inject crashes around journal and file transitions.
3. Implement a read-only IMAP inventory and staged download path. Preserve
   raw message bytes and verify behavior on an isolated test account.
4. Publish incoming messages and integrate recorded Sieve decisions, including
   nested folders and copied destinations.
5. Add external local flag changes, then uploads, moves/copies, and finally
   deletion. Each needs recovery and conflict tests before live enablement.
6. Add IDLE, efficient resynchronization, native filesystem observation, and
   operational controls around the already-correct reconciliation path.
7. Design migration from the existing mbsync-managed Maildir and state. Run
   comparisons and controlled trials before transferring write ownership.

IDLE and filesystem events can be prototyped early, but notification delivery
must not become a substitute for a correct reconciliation engine.

## Acceptance tests

Test useful end states rather than merely mirroring individual functions:

- Restart after remote APPEND/MOVE succeeded but before local completion was
  recorded; handle both lost responses and acknowledged results.
- Restart after local publication succeeded but before its journal update.
- Repeat an arrival observation or lose filesystem events; do not duplicate
  a completed Sieve copy or refile completed imports.
- Change flags on both sides while disconnected; enforce the chosen policy.
- Move a local message while another client deletes it remotely.
- Reset remote UIDVALIDITY or recreate a folder with the same name.
- Lose credentials or the network, then recover without silently stalled work.
- Run alongside an MUA that acknowledges and renames Maildir files.
- Fail a rule or one destination publication without dropping the original.
- Observe a large mailbox with bounded descriptors, memory, and repeated work.

An isolated protocol server and fault injection are needed alongside a small
real-provider test mailbox. Never use the user's production mailbox as a
failure-injection fixture.

## Folder lifecycle implementation

Stable account-bound local marker files distinguish directory renames from
message moves and carry the identity of empty folders. The namespace reconciler
runs before message inventory, using complete LIST observations. It journals
RENAME and child-first DELETE separately from message APPEND/COPY/MOVE. Local
changes require remote_writes; confirmed remote disappearance removes unchanged
local mailbox data, with pending edits treated as conflicts.

There is deliberately no archive, secondary recovery namespace, retained body
copy, or restore workflow for folder removal. Keep operation journaling: it addresses uncertain outcomes,
not data recovery. Metadata rebinds/retirements commit atomically in SQLite.
Retired records prevent config or old rules resurrecting deleted names.

Remote renames cannot be inferred reliably without stable server mailbox IDs.
The current adapter uses removal plus bootstrap import, keeping the UID identity
invariants intact. User-issued local renames have recorded provenance and can
rebind unchanged UIDs after validating epochs and body bytes. INBOX's special
RENAME semantics are intentionally rejected. Folder deletions are distinct from
individual-message deletion and targeted UID EXPUNGE, described below.

## Message deletion implementation

Maildir disappearance and the T flag request deletion only for mapped entries.
Journal intent and confirm it in the same cycle, after a fresh complete scan
and source inventory. There is no deliberate extra-cycle delay. An incomplete mailbox
core is a failed observation, not missing mail. Preserve ambiguous copies and
moves rather than deleting a remote membership by guessing. Unsent intents
cancel if a local file reappears or moves; sent/uncertain intents pause when
local intent changes.

UIDPLUS is mandatory. The adapter verifies UIDVALIDITY and observed portable
flags before UID STORE +Deleted and single-UID EXPUNGE. Never issue EXPUNGE or
CLOSE. The journal can retry an uncertain deletion of the same UID safely after
inventory: UIDs cannot be reused within that epoch. Absence proves completion.
Uncertain native MOVE can retry if a fresh metadata lookup confirms that its
original source UID still exists in the same UIDVALIDITY. Missing sources and
failed checks remain paused without guessing a destination mapping. APPEND
and COPY still require explicit resolution and body verification.

Remote disappearance tombstones the membership and removes only its unchanged
home copy. Changed bodies/flags, copies, and moves retain their local bytes as
conflicts. A restored file observed after an uncertain deletion stays protected
until it is explicitly marked T or removed. Remote MOVE remains removal/import without identity heuristics;
local managed-key moves retain their existing native MOVE path. No archive,
restore command, extra deletion config, or secondary database is introduced.
Observed conflict checks are not atomic with concurrent clients; conditional
STORE protection and larger-scale optimization remain future work.
