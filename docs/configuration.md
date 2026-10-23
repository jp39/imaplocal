# Configuration

[README](../README.md) · [Commands](usage.md) · [Operations and recovery](operations.md)

## Configuration discovery

`-config FILE` explicitly selects a configuration and overrides discovery.
Without it, every command searches these locations in order:

1. `$XDG_CONFIG_HOME/imaplocal/config.json`, when `XDG_CONFIG_HOME` is absolute.
2. `~/.config/imaplocal/config.json`.
3. The platform's user config directory plus `imaplocal/config.json`: on macOS,
   `~/Library/Application Support/imaplocal/config.json`; on Linux,
   `$XDG_CONFIG_HOME/imaplocal/config.json` or `~/.config/imaplocal/config.json`.
4. `~/.imaplocal.json`.

Duplicate paths are searched once. The first existing file wins; missing files
are skipped, while invalid/unreadable files, directories, and dangling symlinks
produce errors instead of selecting another account. Regular-file symlinks are
supported. If no file exists, the error lists the searched locations.

Named account files can still be selected with `-config`; they are not scanned
automatically. Relative paths inside a discovered config are resolved against
its directory, just as with an explicit config. Discovery does not create files,
execute credentials, or initialize a store.

## Initial setup

Copy [the example configuration](../examples/config.json) to a private location
and adjust its host, user, credential command, and paths. Relative paths are
resolved against the configuration file's directory. Unknown configuration
keys are errors. `~` expansion is supported for filesystem paths, not executable
arguments.

The Maildir must be new and empty on first use. Sync metadata and staged
messages live in its private `.imaplocal/` subdirectory, created with mode 0700.
Only `maildir` is configured; there is no separate `state_dir` setting.
An ownership marker binds the Maildir to the state database and account identity,
independently of its absolute path. Existing unmanaged Maildirs, including
mbsync-managed ones, are rejected. Existing imaplocal stores can be
[moved or copied intact](operations.md#moving-or-copying-a-complete-store).

See [store layout and migration](operations.md#store-layout) before changing an
existing store. Configuration and rules are loaded at startup; restart after
editing them. Do not delete state to force reprocessing.

## Folder selection

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
rather than being silently ignored. See [folder renames and removals](operations.md#folder-renames-and-removals)
for lifecycle behavior. Previously unprocessed messages in `filter_folders` are
filtered even on the first inventory of a newly discovered folder.

## Filtering and remote writes

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

For message-size limits and unavailable bodies, see
[download failures](operations.md#download-failures).
