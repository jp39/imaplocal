package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
	"github.com/jp39/imaplocal/internal/status"
)

type deletingFake struct {
	*fake
	deletes               int
	failBefore, failAfter bool
	beforeSnapshot        func()
}

func (f *deletingFake) Snapshot(ctx context.Context, folder string) (model.Mailbox, error) {
	if f.beforeSnapshot != nil {
		hook := f.beforeSnapshot
		f.beforeSnapshot = nil
		hook()
	}
	return f.fake.Snapshot(ctx, folder)
}

func (f *deletingFake) DeleteMessage(_ context.Context, r model.Ref, flags model.Flags) error {
	f.deletes++
	if f.failBefore {
		f.failBefore = false
		return errors.New("lost connection before STORE")
	}
	if f.epochs[r.Folder] != r.Validity {
		return errors.New("stale epoch")
	}
	if s, ok := f.boxes[r.Folder][r.UID]; ok && s.message.Flags != flags {
		return errors.New("changed remote flags")
	}
	delete(f.boxes[r.Folder], r.UID)
	if f.failAfter {
		f.failAfter = false
		return errors.New("lost EXPUNGE response")
	}
	return nil
}
func deletionSetup(t *testing.T) (*Engine, *deletingFake, model.Ref, model.Local) {
	t.Helper()
	e, f := setup(t, "")
	d := &deletingFake{fake: f}
	e.Remote = d
	ref := f.add("INBOX", fixture, model.Seen)
	cycle(t, e)
	return e, d, ref, scan(t, e)[0]
}
func restartDeletion(t *testing.T, e *Engine, f *deletingFake) *Engine {
	t.Helper()
	c := e.Config
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	next.Remote = f
	t.Cleanup(func() { next.Close() })
	return next
}
func TestLocalDeletionConfirmationAndRestartRecovery(t *testing.T) {
	for _, mode := range []string{"removed", "trashed", "lost-before", "lost-after"} {
		t.Run(mode, func(t *testing.T) {
			e, f, ref, l := deletionSetup(t)
			if mode == "trashed" {
				if err := os.Rename(l.Path, l.Path+"T"); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(l.Path); err != nil {
				t.Fatal(err)
			}
			f.failBefore, f.failAfter = mode == "lost-before", mode == "lost-after"
			cycle(t, e)
			if f.deletes != 1 {
				t.Fatal("deletion was delayed until another cycle")
			}
			if (mode == "removed" || mode == "trashed") && (len(f.boxes[ref.Folder]) != 0 || len(scan(t, e)) != 0) {
				t.Fatal("one cycle did not complete deletion")
			}
			e = restartDeletion(t, e, f)
			cycle(t, e)
			if mode == "lost-before" || mode == "lost-after" {
				e = restartDeletion(t, e, f)
				cycle(t, e)
			}
			cycle(t, e)
			if len(f.boxes[ref.Folder]) != 0 || len(scan(t, e)) != 0 {
				t.Fatal("deletion failed to converge")
			}
			want := 1
			if mode == "lost-before" {
				want = 2
			}
			if f.deletes != want {
				t.Fatalf("deletions=%d, want %d", f.deletes, want)
			}
			r, err := status.Read(e.State)
			if err != nil || len(r.Operations) != 0 || len(r.Issues) != 0 || r.Tombstones != 1 {
				t.Fatalf("unclean status: %+v, %v", r, err)
			}
		})
	}
}
func TestDeletionReadOnlyAndConcurrentChanges(t *testing.T) {
	for _, mode := range []string{"readonly", "remote-flags", "local-flags", "body", "copy", "move", "missing-cur"} {
		t.Run(mode, func(t *testing.T) {
			e, f, ref, l := deletionSetup(t)
			switch mode {
			case "readonly":
				e.Config.RemoteWrites = false
				os.Remove(l.Path)
			case "remote-flags":
				os.Remove(l.Path)
				f.Flags(context.Background(), ref, model.Flagged, 0)
			case "local-flags":
				os.Rename(l.Path, l.Path+"FT")
			case "body":
				os.WriteFile(l.Path, []byte("edited body"), 0600)
				os.Rename(l.Path, l.Path+"T")
			case "copy":
				e.Local.Ensure("Archive")
				os.WriteFile(filepath.Join(e.Local.Root, "Archive", "cur", filepath.Base(l.Path)), fixture, 0600)
				os.Rename(l.Path, l.Path+"T")
			case "move":
				e.Local.Ensure("Archive")
				os.Rename(l.Path, filepath.Join(e.Local.Root, "Archive", "cur", filepath.Base(l.Path)+"T"))
			case "missing-cur":
				os.RemoveAll(filepath.Dir(l.Path))
			}
			for i := 0; i < 3; i++ {
				err := e.Cycle(context.Background())
				if mode == "missing-cur" {
					if err == nil {
						t.Fatal("incomplete Maildir accepted")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if f.deletes != 0 || len(f.boxes["INBOX"]) != 1 {
				t.Fatal("conflict propagated a deletion")
			}
		})
	}
}
func TestReappearanceCancelsUnsentDeletionAndAllowsMove(t *testing.T) {
	e, f, _, l := deletionSetup(t)
	os.Remove(l.Path)
	var entry model.Entry
	if found, err := e.State.Get("entry", l.ID, &entry); err != nil || !found {
		t.Fatal(err)
	}
	e.Local.Ensure("Archive")
	path := filepath.Join(e.Local.Root, "Archive", "cur", filepath.Base(l.Path))
	f.beforeSnapshot = func() {
		if err := os.WriteFile(path, fixture, 0600); err != nil {
			t.Fatal(err)
		}
	}
	msg := f.boxes[entry.Remote.Folder][entry.Remote.UID].message
	if err := e.planDelete(context.Background(), entry, msg, nil); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	cycle(t, e)
	if f.deletes != 0 || len(f.boxes["INBOX"]) != 0 || len(f.boxes["Archive"]) != 1 || len(scan(t, e)) != 1 {
		t.Fatal("reappearance was deleted instead of moved")
	}
}
func TestRemoteDeletionAndMovePreserveConflictingLocalWork(t *testing.T) {
	for _, mode := range []string{"unchanged", "body", "flags", "move", "remote-move"} {
		t.Run(mode, func(t *testing.T) {
			e, f, ref, l := deletionSetup(t)
			switch mode {
			case "body":
				os.WriteFile(l.Path, []byte("local edits"), 0600)
			case "flags":
				e.Local.Rename(l, l.ID, l.Flags|model.Flagged)
			case "move":
				e.Local.Ensure("Archive")
				os.Rename(l.Path, filepath.Join(e.Local.Root, "Archive", "cur", filepath.Base(l.Path)))
			}
			if mode == "remote-move" {
				e.Config.Folders = append(e.Config.Folders, "Archive")
				if _, err := f.Transfer(context.Background(), ref, "Archive", true); err != nil {
					t.Fatal(err)
				}
			} else {
				delete(f.boxes[ref.Folder], ref.UID)
			}
			cycle(t, e)
			cycle(t, e)
			locals := scan(t, e)
			if mode == "unchanged" {
				if len(locals) != 0 {
					t.Fatal("remote deletion left an unchanged copy")
				}
			} else if len(locals) != 1 {
				t.Fatalf("local count=%d", len(locals))
			}
			if mode == "remote-move" && locals[0].Folder != "Archive" {
				t.Fatal("remote move left the old copy")
			}
			if f.deletes != 0 || f.appends != 0 {
				t.Fatal("remote disappearance caused a remote write")
			}
		})
	}
}
func TestDeletionEpochResetNeverRetriesStaleUID(t *testing.T) {
	e, f, _, l := deletionSetup(t)
	os.Remove(l.Path)
	var entry model.Entry
	if found, err := e.State.Get("entry", l.ID, &entry); err != nil || !found {
		t.Fatal(err)
	}
	op := model.Operation{ID: "delete:test-stale-epoch", Kind: "delete", EntryID: l.ID, Source: entry.Remote, Flags: entry.Agreed, Hash: entry.Hash, Phase: "planned"}
	if err := e.State.Put("operation", op.ID, op); err != nil {
		t.Fatal(err)
	}
	f.epochs["INBOX"]++
	if err := e.Cycle(context.Background()); err == nil {
		t.Fatal("epoch reset accepted")
	}
	if f.deletes != 0 {
		t.Fatal("stale UID deleted")
	}
	ops, err := state.List[model.Operation](e.State, "operation")
	if err != nil || len(ops) != 1 || ops[0].Applied {
		t.Fatalf("lost pending operation: %+v %v", ops, err)
	}
}

func TestRestoredLocalFileAfterLostDeletionResponseIsRetained(t *testing.T) {
	e, f, _, l := deletionSetup(t)
	if err := os.Remove(l.Path); err != nil {
		t.Fatal(err)
	}
	f.failAfter = true
	cycle(t, e)
	// The remote mutation succeeded, but the operator restores the local file
	// before the next inventory establishes what happened remotely.
	if err := os.WriteFile(l.Path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	e = restartDeletion(t, e, f)
	cycle(t, e)
	cycle(t, e)
	locals := scan(t, e)
	if len(locals) != 1 || f.appends != 0 || f.deletes != 1 {
		t.Fatal("restored file lost or resurrected remotely")
	}
	report, err := status.Read(e.State)
	if err != nil || len(report.Issues) != 1 || report.Issues[0].Kind != "delete_conflict" {
		t.Fatalf("missing durable conflict: %+v %v", report, err)
	}
	if err = os.Rename(locals[0].Path, locals[0].Path+"T"); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if len(scan(t, e)) != 0 {
		t.Fatal("explicit acceptance did not clear restored copy")
	}
}
