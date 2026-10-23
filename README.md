# imaplocal

imaplocal is a daemon for bidirectional synchronization between an IMAP account
and a local Maildir. It downloads remote mail and propagates local flag changes,
moves, copies, uploads, and deletions back to the server. IMAP IDLE and filesystem
notifications trigger synchronization, and Sieve rules file incoming messages.
Mail clients read and modify ordinary Maildir files; SMTP submission and outgoing
queues remain separate.

Existing unmanaged Maildirs and mbsync state cannot be adopted. Owned imaplocal
stores can be [moved or copied intact](docs/operations.md#moving-or-copying-a-complete-store).

## Features

- Download complete messages while preserving their raw bytes.
- File previously unprocessed messages with Sieve, including first inventory.
- Synchronize flags, managed-key moves/copies, uploads, and deletions.
- Track folder renames/removals and journal interrupted operations for recovery.
- Observe changes through IMAP IDLE and native Maildir notifications, with
  periodic reconciliation and reconnect backoff.

## Quick start

Go 1.26 or later is required. Native macOS notifications require cgo; Linux uses
inotify. SQLite does not require cgo.

```sh
go build -o bin/imaplocal ./cmd/imaplocal
```

Copy [the example config](examples/config.json) and [Sieve rules](examples/filter.sieve)
to `~/.config/imaplocal/`, naming the configuration `config.json`. Set the
server, username, credential command, and paths.
The credential command must print exactly one token/password line; connections
use verified TLS. The Maildir must be new and empty on first use. Keep
`remote_writes: false` and `filter_folders: []` for the initial read-only trial.

```sh
./bin/imaplocal check
./bin/imaplocal run
```

`run` synchronizes immediately, then monitors for changes; no preceding `sync`
is required. Stop with Ctrl-C or SIGTERM. In another terminal:

```sh
./bin/imaplocal status
```

Use `-config FILE` to select another configuration; see the
[default search order](docs/configuration.md#configuration-discovery).
Use `sync` for a single cycle, `filter` to preview or explicitly reapply rules,
and `resolve` for uncertain outcomes that need operator verification. See the
[command reference](docs/usage.md) for options and examples.

## Before enabling writes

- `remote_writes: true` enables changes on the server. COPY, MOVE, and deletion
  require UIDPLUS; moves also require MOVE. Message/folder deletions are
  permanent, with no archive or undo workflow.
- Every previously unprocessed membership in `filter_folders` is eligible for
  filtering, including messages already on the server at first inventory.
  Rule edits do not automatically reprocess completed messages.
- Mail and essential synchronization state live together under the Maildir.
  Back up the entire store, including `.imaplocal/`; do not delete state to
  clear errors. Only one writer can own a store.

Review [configuration](docs/configuration.md) and
[operations and recovery](docs/operations.md) before enabling writes.
Configuration and rules are loaded at startup; restart after editing them.
No background service is installed automatically.

## Documentation

- [Configuration](docs/configuration.md): paths, folder wildcards, Sieve,
  credentials, TLS, and proxies.
- [Commands and diagnostics](docs/usage.md): CLI reference, filter selection,
  scheduling, logs, and status.
- [Operations and recovery](docs/operations.md): Maildir layout, folder/message
  lifecycle, uncertain outcomes, migration, and download failures.
- [Design and limitations](docs/design.md): identity, journaling, Maildir
  compatibility, batching, and current boundaries.
- [Development and testing](docs/development.md): builds, source layout,
  isolated tests, and opt-in provider tests.
- [Provider validation](docs/provider-validation.md): recorded results and
  remaining coverage gaps.

## License

[MIT](LICENSE). Dependencies retain their upstream licenses and notices; see
[dependency licenses](docs/development.md#dependency-licenses).
