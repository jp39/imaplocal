package status

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
)

func TestReadableStatusExplainsBlockedChangesAndUnknownOperations(t *testing.T) {
	now := time.Now().UTC()
	r := Report{Health: Health{State: "waiting", Updated: now, LastSuccess: now}, Entries: 3, Tombstones: 1, Issues: []model.Issue{{Kind: "writes_disabled", Detail: "local flags differ; remote_writes is false"}}, Folders: []FolderProgress{{Name: "INBOX", RemoteMessages: 3, Mapped: 2, LastInventory: now, IDLE: IDLEState{State: "retrying", RetryAt: now.Add(time.Second), Error: "connection interrupted"}}}, FolderOperations: []model.FolderOperation{{ID: "folder-op-id", Kind: "rename", Source: "Old", Destination: "New", Phase: "unknown"}}}
	var out bytes.Buffer
	if err := Text(&out, r, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Mode: read-only", "Last state: waiting", "1 local flag change blocked: remote writes disabled", "Messages: 2 tracked, 1 retired", "INBOX", "IDLE retrying", "connection interrupted", "folder-op-id", "unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in status:\n%s", want, out.String())
		}
	}
	if strings.HasPrefix(out.String(), "{") {
		t.Fatal("status unexpectedly emitted JSON")
	}
}

func TestReadDerivesProgressAndOmitsRetiredFolders(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, folder := range []model.Folder{{ID: "a", Name: "INBOX"}, {ID: "b", Name: "Empty"}, {ID: "c", Name: "Gone", Retired: true}} {
		if err = st.Put("folder", folder.Name, folder); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"INBOX", "Empty", "Gone"} {
		if err = st.Put("folder_progress", name, FolderProgress{Name: name, LastInventory: now, RemoteMessages: 2}); err != nil {
			t.Fatal(err)
		}
	}
	st.Put("entry", "one", model.Entry{ID: "one", Remote: model.Ref{Folder: "INBOX"}})
	st.Put("entry", "two", model.Entry{ID: "two", Remote: model.Ref{Folder: "INBOX"}, Tombstone: true})
	st.Put("idle", "INBOX", IDLEState{Folder: "INBOX", State: "connected", Updated: now})
	r, err := Read(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Folders) != 2 || r.Folders[0].Name != "Empty" || r.Folders[1].Name != "INBOX" || r.Folders[1].Mapped != 1 || r.Folders[1].IDLE.State != "connected" {
		t.Fatalf("incorrect progress: %+v", r.Folders)
	}
	// Repeated reads must not inflate derived counts or mutate the store.
	again, err := Read(st)
	if err != nil || again.Folders[1].Mapped != 1 {
		t.Fatal("status read changed counts", err)
	}
}
