//go:build darwin && cgo

package maildir

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeWatcherDoesNotOpenEveryMessage(t *testing.T) {
	root := t.TempDir()
	cur := filepath.Join(root, "INBOX", "cur")
	if err := os.MkdirAll(cur, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		if err := os.WriteFile(filepath.Join(cur, fmt.Sprintf("old-%d:2,", i)), []byte("body"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	notify := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, root, notify) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("native watcher did not stop")
		}
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-notify:
			after, err := os.ReadDir("/dev/fd")
			if err != nil {
				t.Fatal(err)
			}
			if len(after)-len(before) > 32 {
				t.Fatalf("native watcher opened %d descriptors", len(after)-len(before))
			}
			return
		case <-ticker.C:
			if err := os.WriteFile(filepath.Join(cur, "arrival:2,"), []byte(time.Now().String()), 0600); err != nil {
				t.Fatal(err)
			}
		case <-deadline.C:
			t.Fatal("native watcher did not observe changes")
		}
	}
}
