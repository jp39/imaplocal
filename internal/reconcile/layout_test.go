package reconcile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
)

func TestLegacyStateMovePreservesIdentityAndStaging(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := config.Config{Server: "example.org:993", Username: "test", CredentialCommand: []string{"unused"}, Maildir: filepath.Join(root, "mail"), Folders: []string{"INBOX"}}
	old := filepath.Join(root, "old-state")
	st, err := state.Open(old, false)
	if err != nil {
		t.Fatal(err)
	}
	ident := identity{Owner: model.NewID(), Server: c.Server, Username: c.Username, Root: c.Maildir}
	if err = st.Put("meta", "identity", ident); err != nil {
		t.Fatal(err)
	}
	local, err := maildir.New(c.Maildir, old, ident.Owner)
	if err != nil {
		t.Fatal(err)
	}
	delivery := model.Delivery{ID: model.NewID(), Folder: "INBOX", Flags: model.Seen}
	published, err := local.Publish(delivery, fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	arrival := model.Arrival{ID: model.NewID(), Hash: maildir.Hash(fixture)}
	if err = st.Put("arrival", arrival.ID, arrival); err != nil {
		t.Fatal(err)
	}
	if err = local.Stage(arrival.ID, fixture); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(old, c.StateDir()); err != nil {
		t.Fatal(err)
	}
	e, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Local.Private != c.StateDir() {
		t.Fatal("state is outside Maildir")
	}
	var got model.Arrival
	if found, err := e.State.Get("arrival", arrival.ID, &got); err != nil || !found || got.ID != arrival.ID || got.Hash != arrival.Hash {
		t.Fatalf("arrival lost: %+v, %v", got, err)
	}
	raw, err := e.Local.Staged(arrival.ID)
	if err != nil || string(raw) != string(fixture) {
		t.Fatal("staged body lost", err)
	}
	// A private tree resembling a mailbox is never scanned as mail.
	for _, sub := range []string{"new", "cur", "tmp"} {
		if err = os.MkdirAll(filepath.Join(c.StateDir(), "not-a-mailbox", sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(c.StateDir(), "not-a-mailbox", "new", "draft"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := e.Local.Scan()
	if err != nil || len(entries) != 1 || entries[0].Path != published.Path {
		t.Fatalf("unexpected scan: %+v, %v", entries, err)
	}
	if other, err := Open(c); err == nil {
		other.Close()
		t.Fatal("second writer acquired migrated store")
	}
}

func TestUnifiedLayoutRejectsUnmanagedMailAndPrivateSymlink(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmanaged-mail", true: "private-symlink"}[symlink], func(t *testing.T) {
			root := t.TempDir()
			c := config.Config{Server: "example.org:993", Username: "test", CredentialCommand: []string{"unused"}, Maildir: filepath.Join(root, "mail"), Folders: []string{"INBOX"}}
			if err := os.MkdirAll(c.Maildir, 0700); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Mkdir(filepath.Join(root, "outside"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "outside"), c.StateDir()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(c.Maildir, "draft"), fixture, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if e, err := Open(c); err == nil {
				e.Close()
				t.Fatal("unsafe store accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "outside", "state.sqlite")); !os.IsNotExist(err) {
				t.Fatal("wrote state through symlink", err)
			}
		})
	}
}
