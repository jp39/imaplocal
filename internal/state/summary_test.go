package state

import (
	"path/filepath"
	"testing"

	"github.com/jp39/imaplocal/internal/model"
)

func TestSummaryTracksHistoryAndPendingTransitions(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	changes := []Change{
		{Kind: "entry", ID: "active", Value: model.Entry{ID: "active", Remote: model.Ref{Folder: "INBOX"}}},
		{Kind: "entry", ID: "retired", Value: model.Entry{ID: "retired", Remote: model.Ref{Folder: "INBOX"}, Tombstone: true}},
		{Kind: "entry", ID: "legacy", Value: map[string]any{"Remote": map[string]string{"Folder": "Filed"}}},
		{Kind: "arrival", ID: "done", Value: model.Arrival{Done: true}},
		{Kind: "arrival", ID: "pending", Value: map[string]any{}},
		{Kind: "operation", ID: "arrival-done", Value: model.Operation{ID: "arrival-done", Phase: "done", ArrivalID: "arrival"}},
		{Kind: "operation", ID: "external-done", Value: model.Operation{ID: "external-done", Phase: "done", Applied: true}},
		{Kind: "operation", ID: "external-unapplied", Value: model.Operation{ID: "external-unapplied", Phase: "done"}},
		{Kind: "operation", ID: "unknown", Value: model.Operation{ID: "unknown", Phase: "unknown", Applied: true}},
		{Kind: "operation", ID: "legacy", Value: map[string]any{"ID": "legacy"}},
		{Kind: "folder_operation", ID: "folder-done", Value: model.FolderOperation{ID: "folder-done", Phase: "done"}},
		{Kind: "folder_operation", ID: "folder-pending", Value: model.FolderOperation{ID: "folder-pending", Phase: "planned"}},
	}
	if err = s.Apply(changes); err != nil {
		t.Fatal(err)
	}
	got, err := s.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries != 3 || got.Tombstones != 1 || got.Mapped["INBOX"] != 1 || got.Mapped["Filed"] != 1 || got.PendingArrivals != 1 || len(got.Operations) != 3 || len(got.FolderOperations) != 1 {
		t.Fatalf("incorrect summary: %+v", got)
	}
	ops, err := List[model.Operation](s, "operation")
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	for _, op := range ops {
		if op.Phase != "done" || (op.ArrivalID == "" && !op.Applied) {
			pending++
		}
	}
	if len(got.Operations) != pending {
		t.Fatal("summary changed pending semantics")
	}
	if err = s.Apply([]Change{
		{Kind: "entry", ID: "active", Value: model.Entry{ID: "active", Remote: model.Ref{Folder: "INBOX"}, Tombstone: true}},
		{Kind: "entry", ID: "legacy", Delete: true},
		{Kind: "arrival", ID: "pending", Value: model.Arrival{Done: true}},
		{Kind: "operation", ID: "external-unapplied", Value: model.Operation{Phase: "done", Applied: true}},
		{Kind: "operation", ID: "unknown", Delete: true},
		{Kind: "operation", ID: "legacy", Value: model.Operation{Phase: "done", ArrivalID: "arrival"}},
		{Kind: "folder_operation", ID: "folder-pending", Value: model.FolderOperation{Phase: "done"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries != 2 || got.Tombstones != 2 || len(got.Mapped) != 0 || got.PendingArrivals != 0 || len(got.Operations) != 0 || len(got.FolderOperations) != 0 {
		t.Fatalf("indexes did not follow transitions: %+v", got)
	}
}

func TestSummaryReadOnlyLegacyDatabaseDoesNotCreateIndexes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Put("entry", "one", model.Entry{Remote: model.Ref{Folder: "INBOX"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"entry_summary", "pending_arrival", "pending_operation", "pending_folder_operation"} {
		if _, err = s.db.Exec(`DROP INDEX ` + name); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	view, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	got, err := view.Summary()
	if err != nil || got.Entries != 1 {
		t.Fatalf("legacy read: %+v %v", got, err)
	}
	var count int
	if err = view.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='entry_summary'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("read-only status mutated schema", err)
	}
}
