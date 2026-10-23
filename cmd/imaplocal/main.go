// Command imaplocal coordinates IMAP and Maildir synchronization.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/daemon"
	"github.com/jp39/imaplocal/internal/reconcile"
	"github.com/jp39/imaplocal/internal/sieve"
	"github.com/jp39/imaplocal/internal/state"
	"github.com/jp39/imaplocal/internal/status"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "imaplocal:", err)
		os.Exit(1)
	}
}
func usage(w io.Writer) {
	fmt.Fprintln(w, `Usage: imaplocal <check|sync|run|status|filter|resolve> [-config FILE]
       imaplocal -version

check     Validate configuration and Sieve without connecting or creating state.
sync      Perform one reconciliation cycle.
run       Reconcile continuously, with native notifications and optional IMAP IDLE.
status    Show progress, connection state, issues, and pending operations.
filter    Preview or explicitly reapply Sieve rules to synchronized mail.
resolve   Record an explicitly verified outcome for an uncertain remote operation.

Use a fresh, isolated Maildir. Existing mbsync state is never adopted.
Remote writes require remote_writes=true.

Without -config, search $XDG_CONFIG_HOME/imaplocal/config.json,
~/.config/imaplocal/config.json, the platform config directory's
imaplocal/config.json, then ~/.imaplocal.json (first existing file wins).`)
}
func run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		usage(errOut)
		return errors.New("a subcommand is required")
	}
	if args[0] == "-version" || args[0] == "version" {
		fmt.Fprintln(out, "imaplocal", version)
		return nil
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(out)
		return nil
	}
	command := args[0]
	switch command {
	case "check", "sync", "run", "status", "filter", "resolve":
	default:
		return fmt.Errorf("unknown subcommand %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(errOut)
	path := flags.String("config", "", "JSON configuration file (default: search user config locations)")
	var filterFolders, filterMessageIDs stringFlags
	var filterApply, filterDryRun bool
	var filterSince, filterWithin string
	if command == "filter" {
		flags.StringVar(&filterSince, "since", "", "received on/after YYYY-MM-DD (local midnight) or RFC3339 timestamp")
		flags.StringVar(&filterWithin, "within", "", "received within a lookback, e.g. 10d or 24h; d means 24 hours")
		flags.Var(&filterFolders, "folder", "source folder (repeatable; defaults to filter_folders)")
		flags.Var(&filterMessageIDs, "message-id", "exact Message-ID (repeatable)")
		flags.BoolVar(&filterApply, "apply", false, "apply changes; stop the sync daemon first")
		flags.BoolVar(&filterDryRun, "dry-run", false, "preview only (the default; no IMAP connection)")
	}
	var id *string
	var uid, validity *uint
	var notApplied *bool
	var folder, applied *bool
	if command == "resolve" {
		folder = flags.Bool("folder", false, "resolve an uncertain folder rename/removal")
		applied = flags.Bool("applied", false, "assert that the folder operation completed remotely")
		id = flags.String("id", "", "operation ID from status")
		uid = flags.Uint("uid", 0, "verified destination UID")
		validity = flags.Uint("uidvalidity", 0, "verified destination UIDVALIDITY")
		notApplied = flags.Bool("not-applied", false, "assert no remote effect occurred and allow a retry")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not accepted")
	}
	explicitConfig := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})
	if explicitConfig && *path == "" {
		return errors.New("-config FILE must not be empty")
	}
	if !explicitConfig {
		found, err := config.FindDefault()
		if err != nil {
			return err
		}
		*path = found
	}
	c, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("load configuration %q: %w", *path, err)
	}
	switch command {
	case "check":
		if _, err = sieve.Load(c.Sieve); err != nil {
			return err
		}
		fmt.Fprintln(out, "Configuration and Sieve valid; no connection or state changes made.")
		return nil
	case "sync", "run":
		logger := slog.New(slog.NewTextHandler(errOut, nil))
		return daemon.Run(ctx, c, command == "sync", logger)
	case "filter":
		if filterApply && filterDryRun {
			return errors.New("-apply and -dry-run are mutually exclusive")
		}
		since, err := filterCutoff(filterSince, filterWithin, time.Now())
		if err != nil {
			return err
		}
		return filterCommand(ctx, c, reconcile.FilterOptions{Folders: filterFolders, MessageIDs: filterMessageIDs, Since: since}, filterApply, out)
	case "status":
		st, err := state.Open(c.StateDir(), true)
		if err != nil {
			return err
		}
		defer st.Close()
		report, err := status.Read(st)
		if err != nil {
			return err
		}
		return status.Text(out, report, c.RemoteWrites)
	case "resolve":
		if *folder {
			if *id == "" || *applied == *notApplied || *uid != 0 || *validity != 0 {
				return errors.New("folder resolution requires -id and exactly one of -applied or -not-applied")
			}
			st, err := state.Open(c.StateDir(), false)
			if err != nil {
				return err
			}
			defer st.Close()
			if err = reconcile.ResolveFolder(st, *id, *applied); err != nil {
				return err
			}
			fmt.Fprintln(out, "Folder outcome recorded; the next sync validates and resumes it.")
			return nil
		}
		if *applied {
			return errors.New("-applied requires -folder")
		}
		if *id == "" || *uid > 1<<32-1 || *validity > 1<<32-1 {
			return errors.New("valid -id, -uid, and -uidvalidity are required")
		}
		if *notApplied && (*uid != 0 || *validity != 0) {
			return errors.New("-not-applied cannot be combined with destination UID arguments")
		}
		st, err := state.Open(c.StateDir(), false)
		if err != nil {
			return err
		}
		defer st.Close()
		if err = reconcile.Resolve(st, *id, uint32(*uid), uint32(*validity), *notApplied); err != nil {
			return err
		}
		fmt.Fprintln(out, "Resolution recorded. The next sync verifies a supplied destination before binding it.")
		return nil
	}
	return nil
}
