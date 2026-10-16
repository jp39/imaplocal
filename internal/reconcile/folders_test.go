package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
	"github.com/jp39/imaplocal/internal/status"
)

type namespaceFake struct {
	*discoveringFake
	renames int
	deletes int
	lose    bool
}

func (f *namespaceFake) RenameFolder(_ context.Context, from, to string) error {
	if _, exists := f.boxes[from]; !exists {
		return errors.New("source missing")
	}
	for name := range f.boxes {
		if below(name, to) {
			return errors.New("target exists")
		}
	}
	names := []string{}
	for name := range f.boxes {
		if below(name, from) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		target := to + strings.TrimPrefix(name, from)
		f.boxes[target] = f.boxes[name]
		delete(f.boxes, name)
		f.next[target] = f.next[name]
		delete(f.next, name)
		f.epochs[target] = f.epochs[name]
		delete(f.epochs, name)
		for uid, m := range f.boxes[target] {
			m.message.Ref.Folder = target
			f.boxes[target][uid] = m
		}
	}
	f.renames++
	if f.lose {
		f.lose = false
		return errors.New("RENAME response lost")
	}
	return nil
}
func folderSetup(t *testing.T) (*Engine, *namespaceFake) {
	t.Helper()
	e, f := setup(t, "")
	e.Config.Folders = []string{"*"}
	e.Config.FilterFolders = nil
	f.Ensure(context.Background(), "INBOX")
	r := &namespaceFake{discoveringFake: &discoveringFake{fake: f}}
	e.Remote = r
	return e, r
}
func cleanFolderCycle(t *testing.T, e *Engine) {
	t.Helper()
	cycle(t, e)
	r, err := status.Read(e.State)
	if err != nil || len(r.Issues) > 0 || len(r.Operations) > 0 || len(r.FolderOperations) > 0 || r.PendingArrivals != 0 {
		t.Fatalf("unclean folder reconciliation: %+v %v", r, err)
	}
}
func moveFolder(t *testing.T, e *Engine, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(e.Local.Root, from), filepath.Join(e.Local.Root, to)); err != nil {
		t.Fatal(err)
	}
}

func TestLocalFolderRenameRebindsSubtreeWithoutUploading(t *testing.T) {
	e, f := folderSetup(t)
	f.add("Old", fixture, model.Seen)
	f.add("Old/Child", fixture, 0)
	cleanFolderCycle(t, e)
	before := scan(t, e)
	moveFolder(t, e, "Old", "New")
	cleanFolderCycle(t, e)
	cleanFolderCycle(t, e)
	after := scan(t, e)
	if len(after) != 2 || f.renames != 1 || f.appends != 0 || f.copies != 0 {
		t.Fatal("rename duplicated or uploaded messages")
	}
	ids := map[string]bool{}
	for _, l := range before {
		ids[l.ID] = true
	}
	for _, l := range after {
		if !ids[l.ID] || !below(l.Folder, "New") {
			t.Fatal("rename lost stable identity", l)
		}
	}
	records, _ := state.List[model.Entry](e.State, "entry")
	for _, entry := range records {
		if !below(entry.Remote.Folder, "New") {
			t.Fatal("old remote membership retained")
		}
	}
}

func TestLocalFolderRenameWithLiteralConfigAndReadonlyGate(t *testing.T) {
	e, f := folderSetup(t)
	e.Config.Folders = []string{"Old"}
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	moveFolder(t, e, "Old", "New")
	e.Config.RemoteWrites = false
	if err := e.Cycle(context.Background()); err == nil || f.renames != 0 {
		t.Fatal("readonly rename wrote remotely")
	}
	e.Config.RemoteWrites = true
	cleanFolderCycle(t, e)
	cleanFolderCycle(t, e)
	if _, exists := f.boxes["Old"]; exists {
		t.Fatal("literal config recreated old folder")
	}
	if len(scan(t, e)) != 1 {
		t.Fatal("literal config lost renamed message")
	}
}

func TestLocalFolderRemovalUsesChildFirstDeleteWithoutArchives(t *testing.T) {
	e, f := folderSetup(t)
	f.add("Old", fixture, model.Seen)
	f.add("Old/Child", fixture, 0)
	cleanFolderCycle(t, e)
	if err := os.RemoveAll(filepath.Join(e.Local.Root, "Old")); err != nil {
		t.Fatal(err)
	}
	cleanFolderCycle(t, e)
	cleanFolderCycle(t, e)
	if len(scan(t, e)) != 0 || f.deletes != 2 || f.renames != 0 {
		t.Fatal("folder deletion was repeated or renamed instead")
	}
	if _, exists := f.boxes["Old"]; exists {
		t.Fatal("removed remote parent survives")
	}
	if _, exists := f.boxes["Old/Child"]; exists {
		t.Fatal("removed remote child survives")
	}
	if _, err := os.Stat(filepath.Join(e.Config.StateDir(), "recovery")); !os.IsNotExist(err) {
		t.Fatal("unexpected recovery archive")
	}
	f.Ensure(context.Background(), "Old")
	f.epochs["Old"] = 2
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	if len(scan(t, e)) != 1 {
		t.Fatal("recreated mailbox not imported")
	}
}

func TestRemoteRenameRemovesOldAndImportsNewWithoutGuessingIdentity(t *testing.T) {
	e, f := folderSetup(t)
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	oldID := scan(t, e)[0].ID
	e.Config.RemoteWrites = false
	if err := f.RenameFolder(context.Background(), "Old", "New"); err != nil {
		t.Fatal(err)
	}
	cleanFolderCycle(t, e)
	cleanFolderCycle(t, e)
	locals := scan(t, e)
	if len(locals) != 1 || locals[0].Folder != "New" || locals[0].ID == oldID {
		t.Fatal("guessed remote identity or retained old visible folder", locals)
	}
	if _, err := os.Stat(filepath.Join(e.Local.Root, "Old")); !os.IsNotExist(err) {
		t.Fatal("old local folder still exists")
	}
	if _, err := os.Stat(filepath.Join(e.Config.StateDir(), "recovery")); !os.IsNotExist(err) {
		t.Fatal("unexpected recovery archive")
	}

}

func TestRemoteParentRemovalKeepsSurvivingChild(t *testing.T) {
	e, f := folderSetup(t)
	f.add("Old", fixture, model.Seen)
	f.add("Old/Child", fixture, model.Flagged)
	cleanFolderCycle(t, e)
	delete(f.boxes, "Old")
	cleanFolderCycle(t, e)
	locals := scan(t, e)
	if len(locals) != 1 || locals[0].Folder != "Old/Child" {
		t.Fatal("retiring parent removed surviving inferior folder", locals)
	}
}

func TestUncertainFolderRenameRequiresResolutionAfterRestart(t *testing.T) {
	e, f := folderSetup(t)
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	moveFolder(t, e, "Old", "New")
	f.lose = true
	if err := e.Cycle(context.Background()); err == nil {
		t.Fatal("lost acknowledgement ignored")
	}
	ops, _ := state.List[model.FolderOperation](e.State, "folder_operation")
	if len(ops) != 1 || ops[0].Phase != "unknown" {
		t.Fatal("uncertain rename not journaled")
	}
	config := e.Config
	e.Close()
	reopened, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.Remote = f
	if err = reopened.Cycle(context.Background()); err == nil || f.renames != 1 {
		t.Fatal("uncertain rename retried")
	}
	if err = ResolveFolder(reopened.State, ops[0].ID, true); err != nil {
		t.Fatal(err)
	}
	cleanFolderCycle(t, reopened)
	cleanFolderCycle(t, reopened)
	if f.renames != 1 || len(scan(t, reopened)) != 1 {
		t.Fatal("resolution duplicated rename")
	}
}

func TestRemoteRemovalResumesAfterLocalDeleteBeforeCheckpoint(t *testing.T) {
	e, f := folderSetup(t)
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	var folder model.Folder
	e.State.Get("folder", "Old", &folder)
	op := model.FolderOperation{ID: model.NewID(), Kind: "retire", Source: "Old", Phase: "remote_done", Mappings: []model.FolderMapping{{From: "Old", ID: folder.ID}}}
	if err := e.State.Put("folder_operation", op.ID, op); err != nil {
		t.Fatal(err)
	}
	if err := e.Local.RemoveFolderCore("Old", scan(t, e)); err != nil {
		t.Fatal(err)
	}
	delete(f.boxes, "Old")
	config := e.Config
	e.Close()
	reopened, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.Remote = f
	cleanFolderCycle(t, reopened)
	if len(scan(t, reopened)) != 0 {
		t.Fatal("retirement crash recreated local content")
	}
}

func TestFolderDestinationConflictAndInboxProtection(t *testing.T) {
	for _, inbox := range []bool{false, true} {
		t.Run(map[bool]string{false: "conflict", true: "inbox"}[inbox], func(t *testing.T) {
			e, f := folderSetup(t)
			source := "Old"
			if inbox {
				source = "INBOX"
			}
			f.add(source, fixture, model.Seen)
			cleanFolderCycle(t, e)
			moveFolder(t, e, source, "New")
			if !inbox {
				f.add("New", fixture, 0)
			}
			if err := e.Cycle(context.Background()); err == nil || f.renames != 0 {
				t.Fatal("unsafe rename was sent")
			}
		})
	}
}

func (f *namespaceFake) DeleteFolder(_ context.Context, name string) error {
	for child := range f.boxes {
		if child != name && below(child, name) {
			return &model.FolderRejectedError{Err: errors.New("inferior mailbox still exists")}
		}
	}
	if _, exists := f.boxes[name]; !exists {
		return &model.FolderRejectedError{Err: errors.New("missing mailbox")}
	}
	delete(f.boxes, name)
	delete(f.next, name)
	delete(f.epochs, name)
	f.deletes++
	if f.lose {
		f.lose = false
		return errors.New("DELETE reply lost")
	}
	return nil
}

func TestRemoteRemovalBlocksOnPendingLocalChanges(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(map[bool]string{false: "flags", true: "draft"}[draft], func(t *testing.T) {
			e, f := folderSetup(t)
			f.add("Old", fixture, model.Seen)
			cleanFolderCycle(t, e)
			if draft {
				if err := os.WriteFile(filepath.Join(e.Local.Root, "Old", "cur", "draft:2,D"), fixture, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				l := scan(t, e)[0]
				if _, err := e.Local.Rename(l, l.ID, 0); err != nil {
					t.Fatal(err)
				}
			}
			delete(f.boxes, "Old")
			if err := e.Cycle(context.Background()); err == nil {
				t.Fatal("local edits were erased by remote removal")
			}
			want := 1
			if draft {
				want = 2
			}
			if len(scan(t, e)) != want {
				t.Fatal("local conflict content lost")
			}
		})
	}
}

func TestLocalRemovalReadonlyAndUncertainDelete(t *testing.T) {
	e, f := folderSetup(t)
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	os.RemoveAll(filepath.Join(e.Local.Root, "Old"))
	e.Config.RemoteWrites = false
	if err := e.Cycle(context.Background()); err == nil || f.deletes != 0 {
		t.Fatal("readonly deletion propagated")
	}
	e.Config.RemoteWrites = true
	f.lose = true
	if err := e.Cycle(context.Background()); err == nil || f.deletes != 1 {
		t.Fatal("DELETE lost reply was ignored")
	}
	ops, _ := state.List[model.FolderOperation](e.State, "folder_operation")
	if len(ops) != 1 || ops[0].Phase != "unknown" {
		t.Fatal("uncertain DELETE not journaled")
	}
	if err := e.Cycle(context.Background()); err == nil || f.deletes != 1 {
		t.Fatal("uncertain DELETE retried")
	}
	if err := ResolveFolder(e.State, ops[0].ID, true); err != nil {
		t.Fatal(err)
	}
	cleanFolderCycle(t, e)
	if f.deletes != 1 {
		t.Fatal("resolved DELETE retried")
	}
}

func TestInflightFolderRenameOnRestartRequiresExplicitRetry(t *testing.T) {
	e, f := folderSetup(t)
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	var folder model.Folder
	e.State.Get("folder", "Old", &folder)
	moveFolder(t, e, "Old", "New")
	op := model.FolderOperation{ID: model.NewID(), Kind: "rename", Source: "Old", Destination: "New", Phase: "inflight", Mappings: []model.FolderMapping{{From: "Old", To: "New", ID: folder.ID, Validity: folder.Validity, NextUID: 2}}}
	if err := e.State.Put("folder_operation", op.ID, op); err != nil {
		t.Fatal(err)
	}
	cfg := e.Config
	e.Close()
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.Remote = f
	if err = reopened.Cycle(context.Background()); err == nil || f.renames != 0 {
		t.Fatal("interrupted folder intent retried blindly")
	}
	if err = ResolveFolder(reopened.State, op.ID, false); err != nil {
		t.Fatal(err)
	}
	cleanFolderCycle(t, reopened)
	if f.renames != 1 {
		t.Fatal("verified not-applied intent not retried")
	}
}

func TestFolderRemovalRejectsChangedEpochBeforeDelete(t *testing.T) {
	e, f := folderSetup(t)
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	os.RemoveAll(filepath.Join(e.Local.Root, "Old"))
	f.epochs["Old"] = 2
	if err := e.Cycle(context.Background()); err == nil || f.deletes != 0 {
		t.Fatal("deleted a mailbox from a different epoch")
	}
}

func TestExplicitRemovedFolderCanBeRecreatedWithNewEpoch(t *testing.T) {
	e, f := folderSetup(t)
	e.Config.Folders = []string{"Old"}
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	delete(f.boxes, "Old")
	cleanFolderCycle(t, e)
	f.Ensure(context.Background(), "Old")
	f.epochs["Old"] = 2
	f.add("Old", fixture, model.Seen)
	cleanFolderCycle(t, e)
	if len(scan(t, e)) != 1 {
		t.Fatal("explicit folder recreation ignored")
	}
}

func TestLegacyStoreAdoptsMissingRemoteFolderBeforeInventory(t *testing.T) {
	e, f := setup(t, "")
	e.Config.Folders = []string{"*"}
	e.Config.FilterFolders = nil
	// First create pre-folder-sync state through the backend without LIST.
	e.Config.Folders = []string{"Old"}
	f.add("Old", fixture, model.Seen)
	cycle(t, e)
	e.Config.Folders = []string{"*"}
	e.Config.RemoteWrites = false
	namespace := &namespaceFake{discoveringFake: &discoveringFake{fake: f}}
	e.Remote = namespace
	delete(f.boxes, "Old")
	cleanFolderCycle(t, e)
	if len(scan(t, e)) != 0 || namespace.deletes != 0 {
		t.Fatal("legacy disappearance not retired safely")
	}
}

func TestLegacyMissingLocalFolderNeverAuthorizesRemoteDelete(t *testing.T) {
	e, f := setup(t, "")
	e.Config.Folders = []string{"Old"}
	e.Config.FilterFolders = nil
	f.add("Old", fixture, model.Seen)
	cycle(t, e)
	os.RemoveAll(filepath.Join(e.Local.Root, "Old"))
	namespace := &namespaceFake{discoveringFake: &discoveringFake{fake: f}}
	e.Remote = namespace
	if err := e.Cycle(context.Background()); err == nil || namespace.deletes != 0 {
		t.Fatal("upgrade inferred destructive intent without a folder marker")
	}
}
