# External-provider validation checkpoint

On 2026-10-14, imaplocal was exercised against a dedicated IMAPS test account
through an HTTP CONNECT proxy, with destination TLS verification enabled.
Credentials came from a local command and were not written into the project.
Only fresh `imaplocal-test-*` folders received synthetic messages; no SMTP mail
was sent. Existing Inbox messages were inventoried without modification.

The opt-in protocol test passed:

- APPEND and BODY.PEEK preserve exact message bytes.
- Seen can be added and removed without clearing Flagged.
- UIDPLUS mappings support COPY and native MOVE.
- MOVE removes the source membership and preserves destination content.
- A second connection receives an IDLE notification after an arrival, and a
  subsequent complete inventory observes that arrival.

The opt-in reconciliation test passed with a fresh isolated Maildir and SQLite
state:

- A matching bootstrap message remains in its original folder.
- A later matching message is filed by Sieve and marked Seen.
- Repeated local read/unread changes reach the server; remote flag changes
  reach the local Maildir.
- Managed-key local moves and copies become remote MOVE and COPY operations.
- A new local message uploads even when its bytes match existing messages.
- Raw bytes remain unchanged throughout these operations.
- Closing and reopening state and reconnecting preserves four legitimate
  entries without creating duplicates or leaving unresolved work.

A process-level restart check also passed: after a completed reconciliation,
the CLI daemon was killed with SIGKILL, then a fresh sync reopened the same
state. It reported four entries, zero pending arrivals, zero unfinished
operations, and zero issues. This kill occurred while no mutation was in
progress; it does not establish recovery from a lost provider mutation reply.

One initial reconciliation test attempt stopped because its fixture had not
created an empty local move destination. The fixture was corrected and the
complete test passed in fresh folders. Artifacts from both runs are retained.

The normal race suite and `go vet ./...` also passed after adding these tests.
Default tests skip real providers. See [development.md](development.md#real-provider-tests) for explicit opt-in
commands.

Lost mutation replies, publication crash boundaries, UIDVALIDITY resets, and other
fault cases are currently exercised with isolated servers/fake backends. Real
provider fault injection during mutations, prolonged reconnect/VPN disruptions,
and sustained large-mailbox workloads still need broader coverage. Migration from existing mbsync state remains unimplemented. Message deletion
was added and validated in the later checkpoint below.

The bootstrap filtering exclusion described in this historical checkpoint was
removed on 2026-10-16. Current behavior filters every previously unprocessed
membership in a configured filter folder, including its first inventory;
completed decisions are rerun only through the explicit `filter` command.

## Wildcard discovery, 2026-10-15

Read-only provider wildcard discovery and complete metadata inventory passed for
24 selectable mailboxes, including unsubscribed/empty test mailboxes and the
standard INBOX, Sent, Drafts, Spam, Trash, and OUTBOX folders. No messages were
changed. The existing account configuration was not modified by this check.

Isolated regression tests cover folders created while the daemon is running,
empty Maildir creation, Unicode/nested names, failed LIST observations, unsafe
names, per-folder bootstrap filtering, bounded IDLE selection, and a folder
removed between LIST and SELECT (which must not trigger CREATE).

## Folder lifecycle, 2026-10-15

The isolated suite now covers local subtree renames, read-only gates, child-first
folder DELETE, remote rename as removal/import, preservation of surviving child
folders, pending local edit conflicts, lost RENAME/DELETE replies, metadata
transaction rollback, and process interruption around namespace checkpoints.
There are no folder-removal archives or remote recovery namespace.

Provider validation also passed in newly created synthetic test folders:
a local populated-folder rename preserved its two memberships, and deleting
another populated local folder issued IMAP DELETE successfully. Existing
mail and the production stack were not modified by these tests.

## Message deletion and moves, 2026-10-15

The full race suite includes isolated TLS tests for local MOVE, remote MOVE,
local deletion after restart, remote disappearance, and targeted UID EXPUNGE.
The protocol test leaves an unrelated message already marked \Deleted intact,
rejects missing UIDPLUS before setting Deleted, and rejects stale epochs and
read-only remote deletion. Fake tests cover lost responses before/after the
effect, restored/moved files cancelling unsent deletions, changed local bodies
and flags, remote flag changes, incomplete Maildirs, and UIDVALIDITY resets.

The provider reconciliation test also passed in fresh synthetic folders. In addition
to its previous Sieve/flags/local move/copy/upload/folder lifecycle checks, it
confirmed local message deletion, a remote MOVE with no
stale unchanged local source copy, and remote deletion reaching the Maildir.
The generated namespace was
`imaplocal-test-message-20261015T104226Z-engine-54e350b7`; its private store is
`.local/provider-engine-434762049`. No existing messages, account settings,
running daemon, or production mail files were changed. Real-provider lost
mutation replies remain covered only through isolated fault injection.

Deletion confirmation now uses
a fresh scan within the same cycle instead of waiting for another cycle. The
isolated suite asserts same-cycle completion for physical removal and Maildir
T, and cancellation when a move occurs between the initial and fresh scans.

Same-cycle deletion also passed the provider test in fresh namespace
`imaplocal-test-immediate-delete-20261015T105916Z-engine-429867b0`; retained
isolated store: `.local/provider-engine-1945429700`. The test asserts no pending
delete after a single cycle. Race suite, vet, and the rebuilt macOS binary
passed for this simplification. Main mail stores and daemons were untouched.

Retained local fixture labels use the current project name. These are offline
validation artifacts; their labels are not a claim that the corresponding
remote test folders still exist under those names.
