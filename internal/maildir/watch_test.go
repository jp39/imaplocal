//go:build linux || (darwin && cgo)

package maildir

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchIgnoresPrivateWritesAndReportsMail(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, ".imaplocal", "staging")
	inbox := filepath.Join(root, "INBOX", "new")
	for _, dir := range []string{private, inbox} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	notifications := make(chan struct{}, 32)
	done := make(chan error, 1)
	events := make(chan string, 128)
	go func() {
		done <- WatchEvents(ctx, root, notifications, func(path, operation string) {
			select {
			case events <- path:
			default:
			}
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	// Allow the native stream to start and consume pre-watch creation events.
	time.Sleep(500 * time.Millisecond)
	for len(notifications) > 0 {
		<-notifications
	}
	for i := 0; i < 5; i++ {
		if err = os.WriteFile(filepath.Join(private, "body"), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-notifications:
		t.Fatal("private state triggered a reconciliation")
	case <-time.After(600 * time.Millisecond):
	}
	for len(events) > 0 {
		if privateEvent(root, <-events) {
			t.Fatal("private path reached event logger")
		}
	}
	if err = os.WriteFile(filepath.Join(inbox, "message"), []byte("mail"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notifications:
	case <-time.After(3 * time.Second):
		t.Fatal("mail arrival did not trigger watcher")
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case path := <-events:
			if privateEvent(root, path) {
				t.Fatal("private event leaked")
			}
			if path == filepath.Join(inbox, "message") {
				return
			}
		case <-deadline:
			t.Fatal("event callback did not identify the message filename")
		}
	}
}
