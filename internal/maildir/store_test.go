package maildir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-maildir"
	"github.com/jp39/imaplocal/internal/model"
)

func TestPublicationRecoveryAndTraversal(t *testing.T) {
	root := t.TempDir()
	s, err := New(filepath.Join(root, "mail"), filepath.Join(root, "private"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: a@example.org\r\n\r\nbody\r\n")
	d := model.Delivery{ID: model.NewID(), Folder: "INBOX/Review/Mine", Flags: model.Flagged, Date: time.Unix(12345, 0)}
	l, err := s.Publish(d, raw, map[string][]model.Local{})
	if err != nil {
		t.Fatal(err)
	}
	if l.ModTime != d.Date.UnixNano() {
		t.Fatal("IMAP internal date was not preserved")
	}
	acknowledged := filepath.Join(filepath.Dir(filepath.Dir(l.Path)), "cur", filepath.Base(l.Path))
	if err = os.Rename(l.Path, acknowledged); err != nil {
		t.Fatal(err)
	}
	index, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Publish(d, raw, map[string][]model.Local{d.ID: index})
	if err != nil {
		t.Fatal(err)
	}
	if again.Path != acknowledged {
		t.Fatal("acknowledged publication not recognized")
	}
	all, err := s.Scan()
	if err != nil || len(all) != 1 {
		t.Fatal("publication was duplicated")
	}
	for _, folder := range []string{"../outside", "/absolute", "INBOX/../escape", "INBOX/new", "INBOX//empty"} {
		if err = s.Ensure(folder); err == nil {
			t.Fatalf("unsafe folder accepted: %s", folder)
		}
	}
	outside := filepath.Join(root, "outside")
	os.Mkdir(outside, 0700)
	if err = os.Symlink(outside, filepath.Join(s.Root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err = s.Ensure("linked/escape"); err == nil {
		t.Fatal("symlink escape accepted")
	}
	if _, err = s.Scan(); err == nil {
		t.Fatal("scan accepted a symlink")
	}
}
func TestReadRejectsOversizedAndEditedPublication(t *testing.T) {
	root := t.TempDir()
	s, err := New(filepath.Join(root, "mail"), filepath.Join(root, "private"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	d := model.Delivery{ID: model.NewID(), Folder: "INBOX"}
	l, err := s.Publish(d, []byte("body"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Read(l, 2); err == nil {
		t.Fatal("size limit ignored")
	}
	if err = os.WriteFile(l.Path, []byte("edited"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Publish(d, []byte("body"), map[string][]model.Local{d.ID: {l}}); err == nil {
		t.Fatal("edited publication silently overwritten")
	}
}

// Use aerc's actual Maildir dependency: its Unseen acknowledgement strips new/
// info fields. Seen imports bypass new/ under the mbsync-compatible policy.
func TestAercAcknowledgementPreservesDeliveredFlags(t *testing.T) {
	root := t.TempDir()
	s, err := New(filepath.Join(root, "mail"), filepath.Join(root, "private"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	flags := []model.Flags{0, model.Seen, model.Flagged | model.Seen, model.Answered | model.Seen, model.Draft | model.Seen, model.Forwarded | model.Seen}
	for _, f := range flags {
		d := model.Delivery{ID: model.NewID(), Folder: "INBOX", Flags: f}
		l, err := s.Publish(d, []byte("body"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if f != 0 && filepath.Base(filepath.Dir(l.Path)) != "cur" {
			t.Fatal("flagged message delivered into new")
		}
	}
	folder, _ := s.Folder("INBOX")
	acknowledged, err := maildir.Dir(folder).Unseen()
	if err != nil {
		t.Fatal(err)
	}
	if len(acknowledged) != 1 {
		t.Fatal("only unread deliveries should require acknowledgement")
	}
	scanned, err := s.Scan()
	if err != nil || len(scanned) != len(flags) {
		t.Fatal("acknowledgement lost messages", err)
	}
	got := map[model.Flags]int{}
	for _, l := range scanned {
		got[l.Flags]++
	}
	for _, f := range flags {
		if got[f] != 1 {
			t.Fatalf("lost delivered flags %v: %v", f, got)
		}
	}
}

func TestFlagUpdateBeforeAcknowledgementMovesToCur(t *testing.T) {
	root := t.TempDir()
	s, err := New(filepath.Join(root, "mail"), filepath.Join(root, "private"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.Publish(model.Delivery{ID: model.NewID(), Folder: "INBOX"}, []byte("body"), nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err = s.Rename(l, l.ID, model.Flagged)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(l.Path)) != "cur" {
		t.Fatal("updated flags remain exposed to new acknowledgement")
	}
	folder, _ := s.Folder("INBOX")
	if _, err = maildir.Dir(folder).Unseen(); err != nil {
		t.Fatal(err)
	}
	scanned, err := s.Scan()
	if err != nil || len(scanned) != 1 || scanned[0].Flags != model.Flagged {
		t.Fatal("updated flags lost", err)
	}
}

func TestPublicationMatchesMbsyncSeenPlacement(t *testing.T) {
	root := t.TempDir()
	s, err := New(filepath.Join(root, "mail"), filepath.Join(root, "private"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	for flags := model.Flags(0); flags <= model.AllFlags; flags++ {
		id := model.NewID()
		l, err := s.Publish(model.Delivery{ID: id, Folder: "INBOX", Flags: flags}, []byte("exact bytes"), nil)
		if err != nil {
			t.Fatal(err)
		}
		want := "new"
		if flags&model.Seen != 0 {
			want = "cur"
		}
		if filepath.Base(filepath.Dir(l.Path)) != want {
			t.Fatalf("flags %v placed in wrong directory: %s", flags, l.Path)
		}
		if filepath.Base(l.Path) != "imaplocal-"+id+":2,"+flags.Maildir() {
			t.Fatal("delivery lost flag suffix")
		}
	}
}

func TestClearingSeenKeepsAcknowledgedMessageInCur(t *testing.T) {
	root := t.TempDir()
	s, err := New(filepath.Join(root, "mail"), filepath.Join(root, "private"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.Publish(model.Delivery{ID: model.NewID(), Folder: "INBOX", Flags: model.Seen}, []byte("body"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, flags := range []model.Flags{0, model.Seen, 0} {
		l, err = s.Rename(l, l.ID, flags)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(filepath.Dir(l.Path)) != "cur" || l.Flags != flags {
			t.Fatal("read/unread toggle made message new again")
		}
	}
}

// A stale message path is not proof of deletion: MUA new->cur and flag renames
// are normal while the daemon scans. Keep the underlying filesystem error too.
func TestReadConcurrentRenameRequestsRescan(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "message:2,")
	if err := os.WriteFile(old, []byte("message"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, filepath.Join(root, "message:2,S")); err != nil {
		t.Fatal(err)
	}
	_, err := Read(model.Local{Path: old}, 1024)
	if !errors.Is(err, ErrChanged) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("not a retryable observation: %v", err)
	}
}
