package imap

import (
	"bytes"
	"context"
	protocol "github.com/emersion/go-imap/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jp39/imaplocal/internal/auth"
	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/model"
)

// TestProvider is intentionally opt-in. It never uses the user's default
// configuration, mailbox, or OAuth helper unless explicitly supplied.
func TestProvider(t *testing.T) {
	path := os.Getenv("IMAPLOCAL_PROVIDER_CONFIG")
	if path == "" {
		t.Skip("set IMAPLOCAL_PROVIDER_CONFIG for an explicitly authorized test account")
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := os.Getenv("IMAPLOCAL_PROVIDER_WRITE_PREFIX")
	c.RemoteWrites = prefix != ""
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	backend, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	selected := map[string]bool{}
	if c.NeedsDiscovery() {
		folders, e := backend.ListFolders(ctx)
		if e != nil {
			t.Fatal(e)
		}
		for _, f := range folders {
			if c.SelectsFolder(f) {
				selected[f] = true
			}
		}
	}
	for _, f := range c.Folders {
		if !config.FolderPattern(f) {
			selected[f] = true
		}
	}
	for _, folder := range model.SortedFolders(selected) {
		box, err := backend.Snapshot(ctx, folder)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: UIDVALIDITY=%d, messages=%d", folder, box.Validity, len(box.Messages))
	}
	if prefix == "" {
		return
	}
	if !strings.HasPrefix(strings.ToLower(prefix), "imaplocal-test-") {
		t.Fatal("write prefix must start with imaplocal-test- and identify fresh test folders")
	}
	if err = config.Folder(prefix); err != nil {
		t.Fatal(err)
	}
	// Refuse to reuse test folders: this avoids touching a previous run's state.
	for _, name := range []string{prefix + "/Source", prefix + "/Copy", prefix + "/Move"} {
		boxes, err := backend.client.List("", name, nil).Collect()
		if err != nil {
			t.Fatal(err)
		}
		for _, box := range boxes {
			if box.Mailbox == name {
				t.Fatalf("test folder already exists: %s", name)
			}
		}
	}
	for _, name := range []string{prefix + "/Source", prefix + "/Copy", prefix + "/Move"} {
		if err = backend.Ensure(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	raw := []byte("From: imaplocal-test@example.invalid\r\nTo: imaplocal-test@example.invalid\r\nSubject: imaplocal synthetic synchronization test\r\n\r\nThis message was appended through IMAP, not sent by SMTP.\r\n")
	ref, err := backend.Append(ctx, prefix+"/Source", raw, model.Flagged, time.Now())
	if err != nil {
		t.Fatal("APPEND outcome may be uncertain; inspect the dedicated test folder:", err)
	}
	got, err := backend.Fetch(ctx, ref, c.MaxMessageBytes)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("body was changed by provider", err)
	}
	if err = backend.Flags(ctx, ref, model.Seen, 0); err != nil {
		t.Fatal(err)
	}
	if err = backend.Flags(ctx, ref, 0, model.Seen); err != nil {
		t.Fatal(err)
	}
	current, err := backend.Metadata(ctx, ref)
	if err != nil || current.Flags != model.Flagged {
		t.Fatal("portable flag round trip failed", err)
	}
	copied, err := backend.Transfer(ctx, ref, prefix+"/Copy", false)
	if err != nil {
		t.Fatal("COPY outcome may be uncertain; inspect test folders:", err)
	}
	moved, err := backend.Transfer(ctx, ref, prefix+"/Move", true)
	if err != nil {
		t.Fatal("MOVE outcome may be uncertain; inspect test folders:", err)
	}
	for _, r := range []model.Ref{copied, moved} {
		got, err = backend.Fetch(ctx, r, c.MaxMessageBytes)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("transferred content mismatch", err)
		}
	}
	source, err := backend.Snapshot(ctx, prefix+"/Source")
	if err != nil || len(source.Messages) != 0 {
		t.Fatal("MOVE did not remove the source membership", err)
	}
	// Arm IDLE before appending on another connection, excluding SELECT updates.
	notify := make(chan struct{}, 16)
	watcher, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, notify)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if !watcher.client.Caps().Has(protocol.CapIdle) {
		t.Fatal("provider does not advertise IDLE")
	}
	if err = watcher.run(ctx, func() error {
		_, e := watcher.client.Select(prefix+"/Move", &protocol.SelectOptions{ReadOnly: true}).Wait()
		return e
	}); err != nil {
		t.Fatal(err)
	}
	idle, err := watcher.client.Idle()
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	for len(notify) > 0 {
		<-notify
	}
	if _, err = backend.Append(ctx, prefix+"/Move", raw, 0, time.Now()); err != nil {
		t.Fatal("IDLE arrival APPEND may be uncertain:", err)
	}
	select {
	case <-notify:
	case <-time.After(20 * time.Second):
		t.Fatal("no IDLE arrival notification")
	}
	if err = idle.Close(); err != nil {
		t.Fatal(err)
	}
	if err = idle.Wait(); err != nil {
		t.Fatal(err)
	}
	observed, err := watcher.Snapshot(ctx, prefix+"/Move")
	if err != nil || len(observed.Messages) != 2 {
		t.Fatal("IDLE notification did not lead to complete inventory", err)
	}
	t.Log("IDLE arrival notification and catch-up inventory passed")
	t.Logf("Round trip passed; test messages retained under %s for inspection", prefix)
}
