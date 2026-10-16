package imap

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap/v2"
	"github.com/jp39/imaplocal/internal/auth"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/testutil"
)

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return l.Reader.Size() }

func TestTLSIMAPRoundTripAndIDLE(t *testing.T) {
	c, user, ready := testutil.IMAP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	raw := []byte("From: a@example.org\r\nSubject: Exact bytes\r\n\r\nbody\r\n")
	ref, err := backend.Append(ctx, "INBOX", raw, model.Flagged, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	box, err := backend.Snapshot(ctx, "INBOX")
	if err != nil || len(box.Messages) != 1 {
		t.Fatalf("inventory: %+v, %v", box, err)
	}
	body, err := backend.Fetch(ctx, ref, 1<<20)
	if err != nil || !bytes.Equal(body, raw) {
		t.Fatalf("body roundtrip: %q, %v", body, err)
	}
	if err = backend.Flags(ctx, ref, model.Seen, 0); err != nil {
		t.Fatal(err)
	}
	if err = backend.Flags(ctx, ref, 0, model.Seen); err != nil {
		t.Fatal(err)
	}
	box, err = backend.Snapshot(ctx, "INBOX")
	if err != nil || box.Messages[0].Flags != model.Flagged {
		t.Fatal("flag clear failed", err)
	}
	if err = backend.Ensure(ctx, "INBOX/Nested"); err != nil {
		t.Fatal(err)
	}
	copyRef, err := backend.Transfer(ctx, ref, "INBOX/Nested", false)
	if err != nil || !copyRef.Valid() {
		t.Fatal("copy", err)
	}
	if err = backend.Ensure(ctx, "Archive"); err != nil {
		t.Fatal(err)
	}
	moveRef, err := backend.Transfer(ctx, ref, "Archive", true)
	if err != nil || !moveRef.Valid() {
		t.Fatal("move", err)
	}
	box, err = backend.Snapshot(ctx, "INBOX")
	if err != nil || len(box.Messages) != 0 {
		t.Fatal("move left source", err)
	}
	// A second authenticated TLS connection observes an unsolicited IDLE event.
	notify := make(chan struct{}, 10)
	watcher, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, notify)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- watcher.Watch(watchCtx, "INBOX") }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("IDLE did not start")
	}
	for len(notify) > 0 {
		<-notify
	}
	deadline := time.After(5 * time.Second)
	// Repeated arrival simulation avoids relying on a timing sleep for IDLE setup.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	observed := false
	for !observed {
		select {
		case <-notify:
			observed = true
		case <-ticker.C:
			if _, err = user.Append("INBOX", literal{bytes.NewReader(raw)}, &protocol.AppendOptions{}); err != nil {
				t.Fatal(err)
			}
		case err = <-done:
			t.Fatalf("IDLE exited early: %v", err)
		case <-deadline:
			t.Fatal("IDLE did not notify")
		}
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("IDLE did not stop on cancellation")
	}
}
func TestTLSVerificationCannotBeBypassed(t *testing.T) {
	c, _, _ := testutil.IMAP(t)
	c.CAFile = ""
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err == nil {
		b.Close()
		t.Fatal("untrusted certificate accepted")
	}
}
func TestXOAUTH2WireFormat(t *testing.T) {
	mechanism, response, err := (xoauth2{"user@example.org", "token"}).Start()
	if err != nil || mechanism != "XOAUTH2" || string(response) != "user=user@example.org\x01auth=Bearer token\x01\x01" {
		t.Fatal("incorrect SASL initial response")
	}
}

func TestOversizedFetchAbortsWithoutDeadlockingDecoder(t *testing.T) {
	c, _, _ := testutil.IMAP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	backend, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	raw := append([]byte("From: a@example.org\r\n\r\n"), bytes.Repeat([]byte("x"), 1<<20)...)
	ref, err := backend.Append(ctx, "INBOX", raw, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := backend.Fetch(ctx, ref, 16); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("oversized fetch accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("oversized fetch deadlocked while closing a live literal")
	}
}

func TestFolderDiscoveryIncludesUnsubscribedEmptyAndNestedMailboxes(t *testing.T) {
	c, user, _ := testutil.IMAP(t)
	for _, name := range []string{"Empty", "Parent/Child", "日本語"} {
		if err := user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	folders, err := b.ListFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"INBOX": true, "Empty": true, "Parent/Child": true, "日本語": true}
	if len(folders) != len(want) {
		t.Fatalf("discovered folders: %v", folders)
	}
	for _, name := range folders {
		if !want[name] {
			t.Fatal("unexpected folder", name)
		}
	}
	// Unsafe destinations must stop discovery rather than leak into Maildir paths.
	if err = user.Create("INBOX/new", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = b.ListFolders(ctx); err == nil {
		t.Fatal("unsafe mailbox accepted")
	}
}

func TestTLSFolderRenamePreservesContentAndProtectsExistingTargets(t *testing.T) {
	c, _, _ := testutil.IMAP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err = backend.Ensure(ctx, "Old"); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: folder rename\r\n\r\nexact body\r\n")
	ref, err := backend.Append(ctx, "Old", raw, model.Seen, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = backend.RenameFolder(ctx, "Old", "New"); err != nil {
		t.Fatal(err)
	}
	box, err := backend.Snapshot(ctx, "New")
	if err != nil || len(box.Messages) != 1 || box.Validity != ref.Validity {
		t.Fatal("rename lost UID mapping", err)
	}
	body, err := backend.Fetch(ctx, box.Messages[0].Ref, 1<<20)
	if err != nil || !bytes.Equal(body, raw) {
		t.Fatal("rename changed body", err)
	}
	if err = backend.RenameFolder(ctx, "New", "INBOX"); err == nil {
		t.Fatal("INBOX target accepted")
	}
	if err = backend.RenameFolder(ctx, "INBOX", "RenamedInbox"); err == nil {
		t.Fatal("special INBOX rename allowed")
	}
	if err = backend.Ensure(ctx, "Existing"); err != nil {
		t.Fatal(err)
	}
	if err = backend.RenameFolder(ctx, "New", "Existing"); err == nil {
		t.Fatal("existing folder overwritten")
	}
	if err = backend.DeleteFolder(ctx, "New"); err != nil {
		t.Fatal(err)
	}
	if _, err = backend.Snapshot(ctx, "New"); err == nil {
		t.Fatal("deleted mailbox still selectable")
	}
	if err = backend.DeleteFolder(ctx, "INBOX"); err == nil {
		t.Fatal("INBOX deletion permitted")
	}
	if err = backend.DeleteFolder(ctx, "Missing"); err == nil {
		t.Fatal("nonexistent DELETE succeeded")
	} else {
		var rejected *model.FolderRejectedError
		if !errors.As(err, &rejected) {
			t.Fatal("tagged NO misclassified as uncertain", err)
		}
	}
	backend.writes = false
	if err = backend.DeleteFolder(ctx, "Existing"); err == nil {
		t.Fatal("readonly DELETE permitted")
	}
	if err = backend.RenameFolder(ctx, "New", "ReadOnly"); err == nil {
		t.Fatal("readonly folder rename allowed")
	}
}

func TestTargetedDeletionLeavesOtherDeletedMessages(t *testing.T) {
	c, _, _ := testutil.IMAP(t)
	ctx := context.Background()
	backend, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	raw := []byte("Subject: targeted deletion\r\n\r\nbody\r\n")
	target, err := backend.Append(ctx, "INBOX", raw, model.Seen, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other, err := backend.Append(ctx, "INBOX", raw, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = backend.selectRef(other); err != nil {
		t.Fatal(err)
	}
	if err = backend.client.Store(uid(other.UID), &protocol.StoreFlags{Op: protocol.StoreFlagsAdd, Flags: []protocol.Flag{protocol.FlagDeleted}, Silent: true}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	// UID zero must never become an IMAP wildcard deletion.
	invalid := target
	invalid.UID = 0
	if err = backend.DeleteMessage(ctx, invalid, model.Seen); err == nil {
		t.Fatal("invalid deletion UID accepted")
	}
	// A stale expected state must be rejected before setting Deleted.
	if err = backend.DeleteMessage(ctx, target, 0); err == nil {
		t.Fatal("concurrent flags ignored")
	}
	stale := target
	stale.Validity++
	if err = backend.DeleteMessage(ctx, stale, model.Seen); err == nil {
		t.Fatal("stale UIDVALIDITY accepted")
	}
	backend.writes = false
	if err = backend.DeleteMessage(ctx, target, model.Seen); err == nil {
		t.Fatal("read-only deletion accepted")
	}
	backend.writes = true
	for i := 0; i < 2; i++ {
		if err = backend.DeleteMessage(ctx, target, model.Seen); err != nil {
			t.Fatal(err)
		}
	}
	box, err := backend.Snapshot(ctx, "INBOX")
	if err != nil || len(box.Messages) != 1 || box.Messages[0].Ref != other {
		t.Fatalf("unrelated deleted message expunged: %+v %v", box, err)
	}
}

func TestDeletionRequiresUIDPlus(t *testing.T) {
	c, user, _ := testutil.IMAPWithCaps(t, protocol.CapSet{protocol.CapIMAP4rev1: {}})
	raw := []byte("Subject: preserve\r\n\r\nbody\r\n")
	data, err := user.Append("INBOX", literal{bytes.NewReader(raw)}, &protocol.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	backend, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	ref := model.Ref{Folder: "INBOX", Validity: data.UIDValidity, UID: uint32(data.UID)}
	if err = backend.DeleteMessage(ctx, ref, 0); err == nil {
		t.Fatal("unsafe deletion without UIDPLUS")
	}
	box, err := backend.Snapshot(ctx, "INBOX")
	if err != nil || len(box.Messages) != 1 {
		t.Fatalf("unsupported deletion changed mailbox: %+v %v", box, err)
	}
	messages, err := backend.client.Fetch(uid(ref.UID), &protocol.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil || len(messages) != 1 {
		t.Fatal(err)
	}
	for _, flag := range messages[0].Flags {
		if flag == protocol.FlagDeleted {
			t.Fatal("unsupported deletion marked message Deleted")
		}
	}
}
