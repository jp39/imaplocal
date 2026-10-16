package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jp39/imaplocal/internal/model"
)

type discoveringFake struct {
	*fake
	listError error
}

func (f *discoveringFake) ListFolders(context.Context) ([]string, error) {
	if f.listError != nil {
		return nil, f.listError
	}
	set := map[string]bool{}
	for folder := range f.boxes {
		set[folder] = true
	}
	return model.SortedFolders(set), nil
}

func TestWildcardDiscoversEmptyAndNewFoldersAndFiltersInitialMessages(t *testing.T) {
	e, f := setup(t, `require ["fileinto"]; fileinto "Filed";`)
	e.Config.Folders = []string{"*"}
	f.Ensure(context.Background(), "INBOX")
	f.Ensure(context.Background(), "Empty/Nested")
	backend := &discoveringFake{fake: f}
	e.Remote = backend
	cycle(t, e)
	if _, err := os.Stat(filepath.Join(e.Local.Root, "Empty", "Nested", "cur")); err != nil {
		t.Fatal("empty remote folder not represented", err)
	}
	f.add("Later", fixture, 0)
	e.Config.FilterFolders = []string{"Later"}
	cycle(t, e)
	locals := scan(t, e)
	if len(locals) != 1 || locals[0].Folder != "Filed" {
		t.Fatal("new folder initial message not filtered", locals)
	}
	f.add("Later", fixture, 0)
	cycle(t, e)
	locals = scan(t, e)
	if len(locals) != 2 {
		t.Fatal("later arrival missing")
	}
	filed := false
	for _, l := range locals {
		if l.Folder == "Filed" {
			filed = true
		}
	}
	if !filed {
		t.Fatal("new arrival in discovered folder was not filtered")
	}
	// A failed LIST must not interpret tracked memberships as remote deletions.
	backend.listError = errors.New("LIST interrupted")
	if err := e.Cycle(context.Background()); err == nil {
		t.Fatal("LIST error ignored")
	}
	if len(scan(t, e)) != 2 {
		t.Fatal("failed LIST changed local messages")
	}
	backend.listError = nil
	cycle(t, e)
}

func TestWildcardRejectsUnsafeDiscoveredFolderBeforeRemoteMutation(t *testing.T) {
	e, f := setup(t, "")
	e.Config.Folders = []string{"*"}
	f.Ensure(context.Background(), "INBOX/new")
	e.Remote = &discoveringFake{fake: f}
	if err := e.Cycle(context.Background()); err == nil {
		t.Fatal("unsafe discovered folder accepted")
	}
	if len(scan(t, e)) != 0 {
		t.Fatal("unsafe discovery published mail")
	}
}

// A mailbox may disappear between LIST and its first SELECT. Discovery is an
// observation, never permission to undo another client's folder deletion.
type vanishedDiscovery struct{ model.Backend }

func (v vanishedDiscovery) ListFolders(context.Context) ([]string, error) {
	return []string{"Gone"}, nil
}
func (v vanishedDiscovery) Snapshot(context.Context, string) (model.Mailbox, error) {
	return model.Mailbox{}, errors.New("mailbox removed after LIST")
}
func (v vanishedDiscovery) Ensure(context.Context, string) error {
	panic("must not recreate a discovered mailbox")
}
func TestDiscoveryDoesNotRecreateFolderRemovedAfterLIST(t *testing.T) {
	e, f := setup(t, "")
	e.Config.Folders = []string{"*"}
	e.Remote = vanishedDiscovery{f}
	if err := e.Cycle(context.Background()); err == nil {
		t.Fatal("missing discovered mailbox ignored")
	}
	if len(scan(t, e)) != 0 {
		t.Fatal("incomplete observation published mail")
	}
}

type countedDiscovery struct {
	*discoveringFake
	snapshots map[string]int
}

func (f *countedDiscovery) Snapshot(ctx context.Context, name string) (model.Mailbox, error) {
	f.snapshots[name]++
	return f.fake.Snapshot(ctx, name)
}
func TestFolderPatternsDiscoverOnlyMatchingFoldersAndNewChildren(t *testing.T) {
	e, f := setup(t, `require ["fileinto"]; fileinto "INBOX/Filed";`)
	e.Config.Folders = []string{"INBOX", "INBOX/*", "INBOX/Review", "Archive"}
	for _, folder := range []string{"INBOX", "INBOX/Review/My Reviews", "Archive", "Calendar", "Calendar/Birthdays", "Contacts"} {
		f.Ensure(context.Background(), folder)
	}
	backend := &countedDiscovery{discoveringFake: &discoveringFake{fake: f}, snapshots: map[string]int{}}
	e.Remote = backend
	cycle(t, e)
	for _, folder := range []string{"Calendar", "Calendar/Birthdays", "Contacts"} {
		if backend.snapshots[folder] != 0 {
			t.Fatal("unselected folder inventoried", folder)
		}
		if _, err := os.Stat(filepath.Join(e.Local.Root, filepath.FromSlash(folder))); !os.IsNotExist(err) {
			t.Fatal("unselected folder created", folder, err)
		}
	}
	if backend.snapshots["INBOX/Review/My Reviews"] != 1 {
		t.Fatal("nested wildcard or overlapping selector failed", backend.snapshots)
	}
	// No matching folder exists yet: its pattern must never be sent to CREATE.
	e.Config.Folders = append(e.Config.Folders, "Missing/*")
	cycle(t, e)
	if f.boxes["Missing/*"] != nil {
		t.Fatal("created literal wildcard folder")
	}
	f.add("INBOX/Added", fixture, 0)
	e.Config.FilterFolders = []string{"INBOX/Added"}
	cycle(t, e)
	locals := scan(t, e)
	if len(locals) != 1 || locals[0].Folder != "INBOX/Filed" {
		t.Fatal("new child initial message not filtered", locals)
	}
	f.add("INBOX/Added", fixture, 0)
	cycle(t, e)
	if len(scan(t, e)) != 2 || len(f.boxes["INBOX/Filed"]) != 2 {
		t.Fatal("new arrivals in matching child not filtered")
	}
}
func TestNarrowingFolderSelectionPausesTrackedFolders(t *testing.T) {
	e, f := setup(t, "")
	e.Config.Folders = []string{"*"}
	f.add("Calendar", fixture, 0)
	f.add("INBOX", fixture, 0)
	backend := &countedDiscovery{discoveringFake: &discoveringFake{fake: f}, snapshots: map[string]int{}}
	e.Remote = backend
	cycle(t, e)
	var paused model.Local
	for _, l := range scan(t, e) {
		if l.Folder == "Calendar" {
			paused = l
		}
	}
	if paused.ID == "" {
		t.Fatal("missing initial calendar fixture")
	}
	// Local changes and new remote mail in a deselected folder must wait.
	var err error
	paused, err = e.Local.Rename(paused, paused.ID, model.Seen)
	if err != nil {
		t.Fatal(err)
	}
	f.add("Calendar", fixture, 0)
	e.Config.Folders = []string{"INBOX", "INBOX/*"}
	backend.snapshots = map[string]int{}
	cycle(t, e)
	if backend.snapshots["Calendar"] != 0 || len(scan(t, e)) != 2 || f.boxes["Calendar"][1].message.Flags != 0 {
		t.Fatal("deselected folder still synchronized", backend.snapshots)
	}
	// Deselection is not a remote deletion, even if the folder disappears remotely.
	saved := f.boxes["Calendar"]
	delete(f.boxes, "Calendar")
	cycle(t, e)
	if _, err = os.Stat(paused.Path); err != nil {
		t.Fatal("paused local copy removed", err)
	}
	var record model.Entry
	if found, err := e.State.Get("entry", paused.ID, &record); err != nil || !found || record.Tombstone {
		t.Fatal("paused mapping retired", err)
	}
	// Reselecting resumes from the retained mapping, without another upload.
	f.boxes["Calendar"] = saved
	e.Config.Folders = []string{"*"}
	cycle(t, e)
	if len(scan(t, e)) != 3 || f.boxes["Calendar"][1].message.Flags != model.Seen || f.appends != 0 {
		t.Fatal("paused folder failed to resume")
	}
}
func TestDeselectedLocalFolderRemovalDoesNotDeleteRemote(t *testing.T) {
	e, f := setup(t, "")
	e.Config.Folders = []string{"*"}
	f.add("Calendar", fixture, 0)
	f.Ensure(context.Background(), "INBOX")
	e.Remote = &discoveringFake{fake: f}
	cycle(t, e)
	e.Config.Folders = []string{"INBOX", "INBOX/*"}
	if err := os.RemoveAll(filepath.Join(e.Local.Root, "Calendar")); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if len(f.boxes["Calendar"]) != 1 {
		t.Fatal("deselected folder removal propagated")
	}
}
