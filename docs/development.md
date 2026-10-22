# Development and testing

[README](../README.md) · [Design](design.md) · [Provider validation](provider-validation.md)

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

The reconciliation test additionally exercises initial-inventory filtering,
explicit reprocessing, Sieve, bidirectional flags, local moves/copies/uploads,
local and remote message deletion, remote moves, durable-state reopen, and
folder rename/removal in newly created synthetic folders:

```sh
IMAPLOCAL_PROVIDER_CONFIG=/private/path/test-account.json \
IMAPLOCAL_PROVIDER_WRITE_PREFIX=imaplocal-test-UNIQUE \
  go test ./internal/reconcile -run '^TestProviderReconciliation$' -v -timeout 5m
```

It creates a random suffix beneath the supplied remote prefix and retains a
private local test tree beside the supplied config. Its log gives both paths.
Those artifacts contain synthetic mail and account configuration; keep them
private. Real-provider tests supplement isolated crash/fault tests. Results
apply to the tested providers and configurations.

Provider results and remaining coverage limits are recorded in
[provider-validation.md](provider-validation.md).

## Source structure

```text
cmd/imaplocal/         CLI commands and signal handling
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

## Dependency licenses

Dependencies retain their upstream licenses: go-imap and go-sasl (MIT),
go-sieve (MIT), fsnotify/FSEvents (BSD), and modernc SQLite and its dependencies
(see their module license files and third-party notices). Preserve applicable
dependency copyright notices and license texts when redistributing binaries.
