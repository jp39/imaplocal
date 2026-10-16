package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jp39/imaplocal/internal/auth"
	remote "github.com/jp39/imaplocal/internal/imap"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/status"
	"github.com/jp39/imaplocal/internal/testutil"
)

func TestMessageDeletionAndMovesTLS(t *testing.T) {
	c, _, _ := testutil.IMAP(t)
	c.Folders = append(c.Folders, "Archive")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend, err := remote.Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err = backend.Ensure(ctx, "Archive"); err != nil {
		t.Fatal(err)
	}
	if _, err = backend.Append(ctx, "INBOX", fixture, model.Seen, time.Now()); err != nil {
		t.Fatal(err)
	}
	e, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { e.Close() }()
	e.Remote = backend
	cycle(t, e)
	l := scan(t, e)[0]
	// A managed-key local move uses server-side MOVE, not APPEND plus deletion.
	if err = os.Rename(l.Path, filepath.Join(c.Maildir, "Archive", "cur", filepath.Base(l.Path))); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	box, err := backend.Snapshot(ctx, "INBOX")
	if err != nil || len(box.Messages) != 0 {
		t.Fatal("local move left source", err)
	}
	l = scan(t, e)[0]
	if l.Folder != "Archive" {
		t.Fatal("local move mapping failed")
	}
	if err = os.Rename(l.Path, l.Path+"T"); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	box, err = backend.Snapshot(ctx, "Archive")
	if err != nil || len(box.Messages) != 0 || len(scan(t, e)) != 0 {
		t.Fatal("deletion deferred beyond one cycle", err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	e.Remote = backend
	cycle(t, e)
	box, err = backend.Snapshot(ctx, "Archive")
	if err != nil || len(box.Messages) != 0 || len(scan(t, e)) != 0 {
		t.Fatal("local deletion did not converge", err)
	}
	// Remote moves have new UID identities; the unchanged old copy disappears.
	ref, err := backend.Append(ctx, "INBOX", fixture, model.Seen, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	moved, err := backend.Transfer(ctx, ref, "Archive", true)
	if err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	ls := scan(t, e)
	if len(ls) != 1 || ls[0].Folder != "Archive" {
		t.Fatalf("remote move left stale copies: %+v", ls)
	}
	if err = backend.DeleteMessage(ctx, moved, model.Seen); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	cycle(t, e)
	if len(scan(t, e)) != 0 {
		t.Fatal("remote deletion left a local copy")
	}
	report, err := status.Read(e.State)
	if err != nil || len(report.Operations) != 0 || len(report.Issues) != 0 {
		t.Fatalf("unresolved work: %+v %v", report, err)
	}
}
