package daemon

import (
	"bytes"
	"context"
	"github.com/jp39/imaplocal/internal/auth"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap/v2"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
	"github.com/jp39/imaplocal/internal/status"
	"github.com/jp39/imaplocal/internal/testutil"
)

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }
func TestDaemonDownloadsFiltersObservesFlagsAndStops(t *testing.T) {
	c, user, _ := testutil.IMAP(t)
	c.Folders = []string{"*"}
	c.Interval = "2s"
	c.Idle = true
	root := t.TempDir()
	c.Sieve = filepath.Join(root, "filter.sieve")
	c.FilterFolders = []string{"INBOX"}
	script := `require ["fileinto", "imap4flags"]; if header :contains "from" "builds@example.com" { addflag "\\Seen"; fileinto "INBOX/Builds"; stop; }`
	if err := os.WriteFile(c.Sieve, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: builds@example.com\r\nSubject: Build\r\n\r\nexact body\r\n")
	if _, err := user.Append("INBOX", literal{bytes.NewReader([]byte("From: person@example.com\r\nSubject: Existing\r\n\r\nbody\r\n"))}, &protocol.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, false, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon failed to stop")
		}
	})
	wait := func(predicate func(*state.Store, status.Report) bool) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			st, err := state.Open(c.StateDir(), true)
			if err == nil {
				report, e := status.Read(st)
				ok := e == nil && predicate(st, report)
				st.Close()
				if ok {
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("daemon did not converge")
	}
	wait(func(_ *state.Store, r status.Report) bool {
		connected := false
		for _, folder := range r.Folders {
			if folder.Name == "INBOX" && folder.IDLE.State == "connected" && !folder.LastInventory.IsZero() {
				connected = true
			}
		}
		return r.Entries == 1 && r.PendingArrivals == 0 && connected
	})
	// The initial unmatched message stays in Inbox.
	var existing string
	err := filepath.WalkDir(filepath.Join(c.Maildir, "INBOX", "new"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			existing = p
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if existing == "" {
		t.Fatal("bootstrap message missing")
	}
	if _, err = user.Append("INBOX", literal{bytes.NewReader(raw)}, &protocol.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	wait(func(st *state.Store, r status.Report) bool {
		entries, err := state.List[model.Entry](st, "entry")
		if err != nil || r.Entries != 2 || r.PendingArrivals != 0 || len(r.Issues) != 0 {
			return false
		}
		for _, entry := range entries {
			if entry.Remote.Folder == "INBOX/Builds" && entry.Agreed&model.Seen != 0 {
				return true
			}
		}
		return false
	})
	// Simulate aerc acknowledging and marking the original message read.
	if err = os.Rename(existing, filepath.Join(c.Maildir, "INBOX", "cur", filepath.Base(existing)+"S")); err != nil {
		t.Fatal(err)
	}
	wait(func(st *state.Store, r status.Report) bool {
		entries, err := state.List[model.Entry](st, "entry")
		if err != nil {
			return false
		}
		for _, entry := range entries {
			if entry.Remote.Folder == "INBOX" && entry.Agreed&model.Seen != 0 {
				return true
			}
		}
		return false
	})
	if err = user.Create("Later", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = user.Append("Later", literal{bytes.NewReader(raw)}, &protocol.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	wait(func(st *state.Store, r status.Report) bool {
		entries, err := state.List[model.Entry](st, "entry")
		if err != nil || r.Entries != 3 || len(r.Issues) > 0 {
			return false
		}
		for _, entry := range entries {
			if entry.Remote.Folder == "Later" {
				return true
			}
		}
		return false
	})

	// Drive namespace changes through the real daemon, protocol adapter, and
	// filesystem observer. The test server supports leaf RENAME and DELETE.
	if err = os.Rename(filepath.Join(c.Maildir, "Later"), filepath.Join(c.Maildir, "Renamed")); err != nil {
		t.Fatal(err)
	}
	wait(func(st *state.Store, r status.Report) bool {
		if len(r.Issues) > 0 || len(r.FolderOperations) > 0 {
			return false
		}
		entries, err := state.List[model.Entry](st, "entry")
		if err != nil {
			return false
		}
		for _, entry := range entries {
			if entry.Remote.Folder == "Renamed" && !entry.Tombstone {
				return true
			}
		}
		return false
	})
	if err = os.RemoveAll(filepath.Join(c.Maildir, "Renamed")); err != nil {
		t.Fatal(err)
	}
	wait(func(_ *state.Store, r status.Report) bool {
		return r.Tombstones == 1 && len(r.FolderOperations) == 0 && len(r.Issues) == 0
	})

}

func TestIdleSelectionBoundsConnectionsAndPrioritizesInbox(t *testing.T) {
	folders := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "INBOX"}
	selected := idleFolders(folders)
	if len(selected) != 8 || !selected["INBOX"] {
		t.Fatalf("unbounded selection or missing Inbox: %v", selected)
	}
	if folders[0] != "A" {
		t.Fatal("watch selection mutated caller's folder list")
	}
}

func TestReadonlyFlagChangeExplainedInLogAndSavedStatus(t *testing.T) {
	c, user, _ := testutil.IMAP(t)
	c.RemoteWrites = false
	raw := []byte("Subject: status test\r\n\r\nbody\r\n")
	if _, err := user.Append("INBOX", literal{bytes.NewReader(raw)}, &protocol.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if err := Run(ctx, c, true, logger); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(c.Maildir, "INBOX", "new"))
	if err != nil || len(entries) != 1 {
		t.Fatal("bootstrap missing", err)
	}
	old := filepath.Join(c.Maildir, "INBOX", "new", entries[0].Name())
	if err = os.Rename(old, filepath.Join(c.Maildir, "INBOX", "cur", entries[0].Name()+"S")); err != nil {
		t.Fatal(err)
	}
	if err = Run(ctx, c, true, logger); err == nil {
		t.Fatal("blocked flag change unexpectedly succeeded")
	}
	if !strings.Contains(logs.String(), "1 local flag change blocked: remote writes disabled") {
		t.Fatal("warning omitted actual reason", logs.String())
	}
	st, err := state.Open(c.StateDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	report, err := status.Read(st)
	if err != nil {
		t.Fatal(err)
	}
	if report.Health.State != "stopped" || report.Health.LastSuccess.IsZero() || len(report.Folders) != 1 || report.Folders[0].LastInventory.IsZero() || report.Folders[0].Mapped != 1 {
		t.Fatalf("missing saved progress: %+v", report)
	}
}

func TestIdleFailurePublishesRetryThenStopped(t *testing.T) {
	c, _, _ := testutil.IMAP(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c.Server = listener.Addr().String()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan status.IDLEState, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watch(ctx, c, "INBOX", &auth.Command{Args: c.CredentialCommand}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), func(value status.IDLEState) { updates <- value })
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case value := <-updates:
			if value.State != "retrying" {
				continue
			}
			if value.Error == "" || value.RetryAt.IsZero() {
				t.Fatal("retry status lacks reason or schedule")
			}
			cancel()
			select {
			case <-done:
			case <-deadline:
				t.Fatal("watcher did not stop")
			}
			stopped := false
			for len(updates) > 0 {
				if (<-updates).State == "stopped" {
					stopped = true
				}
			}
			if !stopped {
				t.Fatal("watcher left a live-looking status after cancellation")
			}
			return
		case <-deadline:
			t.Fatal("failed TLS connection did not produce retry status")
		}
	}
}
