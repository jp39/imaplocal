# Maildir operations and recovery

[README](../README.md) · [Configuration](configuration.md) · [Commands](usage.md)

Examples use the discovered default config. Add `-config FILE` to operate on a
specific account.

## Store layout

Each Maildir root contains `.imaplocal/owner`, `.imaplocal/state.sqlite`,
`.imaplocal/daemon.lock`, and private staging/quarantine directories. Each
tracked mailbox has an `.imaplocal-folder` marker that travels with folder
renames. Managed message filenames use `imaplocal-<id>:2,<flags>`.
The owner ID and folder/message IDs must agree with the database. Do not
remove or recreate them to repair synchronization.

## Moving or copying a complete store

A store is portable: its absolute filesystem path is not part of its identity.
The database remains bound to the configured server and username, and the
`.imaplocal/owner` marker must match its ownership ID. Older databases may still
contain a `Root` field; it is ignored and requires no migration or manual edit.

To relocate a store, stop its daemon and mail clients, move the entire Maildir
including `.imaplocal/` and all hidden folder markers, and update `maildir` in
the configuration and the mail client's source path. Restart using the new
paths. No state database changes, initial body downloads, or refiltering are
required. Already-journaled work resumes normally.

To seed an independent replica on another machine or in another directory:

1. Stop the source daemon and mail clients, and check `imaplocal status`.
   Finish pending operations and resolve uncertain outcomes before copying, so
   unfinished uploads, copies, or folder changes cannot be replayed twice.
2. Copy the complete Maildir, including hidden files, preserving permissions.
   Copy the database together with any SQLite sidecars; do not copy a live
   database or just the visible message folders.
3. Configure the replica for the same server and username. Adjust `maildir`,
   credential commands, Sieve and other machine-specific paths as needed.
4. Set `filter_folders: []` on additional replicas if another instance already
   runs Sieve filing for the account. Filter decisions are local to each store;
   two independent replicas can both execute a COPY rule for the same arrival.
5. Start the replicas. Each has an independent writer lock and synchronization
   state. Existing remote UID mappings and completed filtering history are
   retained. Normal inventory catches up with the server; only new memberships
   need body downloads.

The ownership marker checks that a Maildir and database belong together. It
is not a globally unique replica ID, and copied stores may keep it unchanged.
One writer per physical store is still required; do not run multiple daemons
against one shared filesystem store.

## Folder renames and removals

Folder identities are recorded on the first successful observation. Each local
Maildir gets a hidden `.imaplocal-folder` marker so directory renames, including
empty folders, can be recognized. Existing state is adopted automatically
from its saved boundaries. If an older store already has a missing local folder
while it still exists remotely, restore the directory before using this feature;
the upgrade will not infer destructive intent without an established marker.
Preserve this marker when renaming a folder;
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
[message deletion policy](#message-deletion-and-moves).

Folder operations are journaled, and namespace metadata changes commit in one
SQLite transaction. An interrupted RENAME/DELETE with an unknown outcome is
not retried. Inspect pending folder operations in `status`, stop the daemon,
and record an independently verified outcome with one of:

```sh
./bin/imaplocal resolve \
  -folder -id OPERATION_ID -applied
./bin/imaplocal resolve \
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
./bin/imaplocal resolve \
  -id OPERATION_ID -uid VERIFIED_UID -uidvalidity VERIFIED_UIDVALIDITY
./bin/imaplocal sync
```

The next cycle fetches that membership and verifies its body hash before
binding it. A mistaken unverified mapping can be corrected with another
`resolve` invocation.

If an operator has established that **no remote effect occurred**, an explicit
resolution allows a retry:

```sh
./bin/imaplocal resolve \
  -id OPERATION_ID -not-applied
```

This is an operator assertion, not an automatic deduction. Do not use it merely
because a message was not immediately visible in another client.

Publication is recovered by the stable Maildir key, including acknowledgement
by the MUA. Completed scratch bodies are reclaimed only after durable completion;
interrupted plans keep their staged content. Never manually delete staging or
the operation journal to clear an error. Back up state and Maildir together:
the database holds essential UID and operation history, not a disposable index.

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
Do not run an older binary against a migrated store. To relocate or duplicate
the complete Maildir, follow [moving or copying a store](#moving-or-copying-a-complete-store).
To back up the store, stop the daemon and copy the entire Maildir, including its
hidden files and `.imaplocal/` directory.

## Download failures

### Unavailable bodies

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

### Message-size limits

If a fetched MIME literal exceeds `max_message_bytes` even though its advertised
RFC822.SIZE was smaller, the actual size is checkpointed before closing the
connection. That first observation may require a reconnect without draining the
large body. Later cycles skip that UID with a `message_too_large` issue and keep
syncing the remaining mail. Increasing the limit above its observed size makes
it eligible again. No empty replacement is published and no remote message is
deleted. The default limit is 64 MiB; accounts with larger mail need a larger
explicit value.

See [batched downloads](design.md#batched-body-downloads) for the download and
checkpointing design.
