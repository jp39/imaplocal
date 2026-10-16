package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jp39/imaplocal/internal/auth"
	"github.com/jp39/imaplocal/internal/config"
	remote "github.com/jp39/imaplocal/internal/imap"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/status"
)

// TestProviderReconciliation is opt-in and confines mutations to fresh synthetic
// folders. Retain its private Maildir/state and remote messages for inspection.
func TestProviderReconciliation(t *testing.T) {
	path, prefix := os.Getenv("IMAPLOCAL_PROVIDER_CONFIG"), os.Getenv("IMAPLOCAL_PROVIDER_WRITE_PREFIX")
	if path == "" || prefix == "" {
		t.Skip("requires explicit provider configuration and write prefix")
	}
	if !strings.HasPrefix(prefix, "imaplocal-test-") {
		t.Fatal("unsafe write prefix")
	}
	if err := config.Folder(prefix); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix += "-engine-" + model.NewID()[:8]
	source, filed, moved, copied := prefix+"/Source", prefix+"/Filed", prefix+"/Moved", prefix+"/Copied"
	root, err := os.MkdirTemp(filepath.Dir(path), "provider-engine-")
	if err != nil {
		t.Fatal(err)
	}
	c.Maildir = filepath.Join(root, "mail")
	c.Folders = []string{source, filed, moved, copied}
	c.FilterFolders = []string{source}
	c.RemoteWrites = true
	c.Sieve = filepath.Join(root, "filter.sieve")
	script := `require ["fileinto", "imap4flags"]; if header :contains "subject" "Filter me" { addflag "\\Seen"; fileinto "` + filed + `"; stop; }`
	if err = os.WriteFile(c.Sieve, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.MarshalIndent(c, "", "  ")
	if err = os.WriteFile(filepath.Join(root, "config.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Dedicated remote root: %s; retained local test tree: %s", prefix, root)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	backend, err := remote.Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	for _, folder := range c.Folders {
		if err = backend.Ensure(ctx, folder); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if engine != nil {
			engine.Close()
		}
	}()
	engine.Remote = backend
	for _, folder := range c.Folders {
		if err = engine.Local.Ensure(folder); err != nil {
			t.Fatal(err)
		}
	}
	cycle := func() {
		t.Helper()
		if err := engine.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
		r, err := status.Read(engine.State)
		if err != nil || len(r.Issues) > 0 || len(r.Operations) > 0 || r.PendingArrivals > 0 {
			t.Fatalf("unresolved cycle: %+v, %v", r, err)
		}
	}
	raw := []byte("From: imaplocal-test@example.invalid\r\nSubject: Filter me\r\n\r\nUnmodified synthetic body.\r\n")
	// An unmatched initial message stays; matching messages are filtered regardless of arrival time.
	historical, err := backend.Append(ctx, source, bytes.Replace(raw, []byte("Filter me"), []byte("Keep me"), 1), 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cycle()
	if _, err = backend.Append(ctx, source, raw, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	cycle()
	locals := func() []model.Local {
		t.Helper()
		ls, err := engine.Local.Scan()
		if err != nil {
			t.Fatal(err)
		}
		return ls
	}
	find := func(folder string) model.Local {
		t.Helper()
		for _, l := range locals() {
			if l.Folder == folder {
				return l
			}
		}
		t.Fatalf("no local entry in %s", folder)
		return model.Local{}
	}
	if len(locals()) != 2 {
		t.Fatal("bootstrap/arrival count mismatch")
	}
	l := find(filed)
	body, err := os.ReadFile(l.Path)
	if err != nil || !bytes.Equal(body, raw) || l.Flags != model.Seen {
		t.Fatal("Sieve download flags/content mismatch", err)
	}
	box, err := backend.Snapshot(ctx, source)
	if err != nil || len(box.Messages) != 1 || box.Messages[0].Ref != historical {
		t.Fatal("unmatched initial message moved or matching arrival retained", err)
	}
	// Emulate aerc moving new to cur, then clearing and setting Seen repeatedly.
	for _, flags := range []model.Flags{0, model.Seen, 0} {
		l = find(filed)
		base := strings.Split(filepath.Base(l.Path), ":2,")[0]
		target := filepath.Join(c.Maildir, filepath.FromSlash(filed), "cur", base+":2,"+flags.Maildir())
		if err = os.Rename(l.Path, target); err != nil {
			t.Fatal(err)
		}
		cycle()
		box, err = backend.Snapshot(ctx, filed)
		if err != nil || len(box.Messages) != 1 || box.Messages[0].Flags != flags {
			t.Fatal("local flag change not propagated", err)
		}
	}
	if err = backend.Flags(ctx, box.Messages[0].Ref, model.Seen|model.Flagged, 0); err != nil {
		t.Fatal(err)
	}
	cycle()
	if find(filed).Flags != model.Seen|model.Flagged {
		t.Fatal("remote flags not reflected locally")
	}
	// Preserve the managed key for a local move, then a legitimate copy.
	l = find(filed)
	if err = os.Rename(l.Path, filepath.Join(c.Maildir, filepath.FromSlash(moved), "cur", filepath.Base(l.Path))); err != nil {
		t.Fatal(err)
	}
	cycle()
	l = find(moved)
	if err = os.WriteFile(filepath.Join(c.Maildir, filepath.FromSlash(copied), "cur", filepath.Base(l.Path)), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cycle()
	// A newly created cur entry is a new upload, even with identical bytes.
	if err = os.WriteFile(filepath.Join(c.Maildir, filepath.FromSlash(source), "cur", "external-test:2,S"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cycle()
	if len(locals()) != 4 {
		t.Fatal("legitimate copy/upload count mismatch")
	}
	// Reopen durable state and reconnect; repeat observations must add no copies.
	if err = engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine = nil
	backend.Close()
	backend, err = remote.Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	engine, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	engine.Remote = backend
	cycle()
	cycle()
	for folder, want := range map[string]int{source: 2, filed: 0, moved: 1, copied: 1} {
		box, err = backend.Snapshot(ctx, folder)
		if err != nil || len(box.Messages) != want {
			t.Fatalf("%s: got %d messages, want %d: %v", folder, len(box.Messages), want, err)
		}
		for _, m := range box.Messages {
			b, err := backend.Fetch(ctx, m.Ref, c.MaxMessageBytes)
			if err != nil || !bytes.Equal(b, raw) {
				t.Fatal("final remote content mismatch", err)
			}
		}
	}
	if len(locals()) != 4 {
		t.Fatal("restart duplicated local messages")
	}
	// Exercise namespace changes only within this run's synthetic folders.
	renamed := prefix + "/RenamedSource"
	if err = os.Rename(filepath.Join(c.Maildir, filepath.FromSlash(source)), filepath.Join(c.Maildir, filepath.FromSlash(renamed))); err != nil {
		t.Fatal(err)
	}
	cycle()
	renamedBox, err := backend.Snapshot(ctx, renamed)
	if err != nil || len(renamedBox.Messages) != 2 {
		t.Fatal("local folder rename failed", err)
	}
	if err = os.RemoveAll(filepath.Join(c.Maildir, filepath.FromSlash(copied))); err != nil {
		t.Fatal(err)
	}
	cycle()
	cycle()
	remaining, err := backend.ListFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range remaining {
		if name == source || name == copied {
			t.Fatal("renamed/deleted source folder still exists", name)
		}
	}
	if len(locals()) != 3 {
		t.Fatal("folder deletion removed the wrong local membership")
	}
	// Local message removal completes in one cycle, with a fresh intent scan.
	l = find(moved)
	if err = os.Remove(l.Path); err != nil {
		t.Fatal(err)
	}
	if err = engine.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	completed, err := status.Read(engine.State)
	if err != nil || len(completed.Operations) != 0 {
		t.Fatalf("deletion was deferred: %+v %v", completed, err)
	}
	box, err = backend.Snapshot(ctx, moved)
	if err != nil || len(box.Messages) != 0 {
		t.Fatal("local message deletion failed", err)
	}
	// Exercise a remote MOVE, followed by a remote targeted deletion, without
	// touching any messages outside this run's generated namespace.
	renamedBox, err = backend.Snapshot(ctx, renamed)
	if err != nil || len(renamedBox.Messages) != 2 {
		t.Fatal("missing move fixture", err)
	}
	remoteMoved, err := backend.Transfer(ctx, renamedBox.Messages[0].Ref, filed, true)
	if err != nil {
		t.Fatal(err)
	}
	cycle()
	if len(locals()) != 2 || find(filed).Folder != filed {
		t.Fatal("remote move did not converge")
	}
	if err = backend.DeleteMessage(ctx, remoteMoved, renamedBox.Messages[0].Flags); err != nil {
		t.Fatal(err)
	}
	cycle()
	if len(locals()) != 1 {
		t.Fatal("remote deletion did not converge")
	}
	t.Log("Local and remote message deletion and remote MOVE passed")
	t.Log("Local folder RENAME and populated-folder DELETE passed; no archives created")
	t.Log("Bootstrap exclusion, Sieve filing, bidirectional flags, local move/copy/upload, exact bytes, and restart passed")
}
