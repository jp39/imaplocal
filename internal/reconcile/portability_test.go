package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
)

type portableBackend struct {
	*fake
	fetches int
}

func (f *portableBackend) Fetch(ctx context.Context, ref model.Ref, limit int64) ([]byte, error) {
	f.fetches++
	return f.fake.Fetch(ctx, ref, limit)
}

func TestCompleteStoreCanMoveOrSeedIndependentReplica(t *testing.T) {
	for _, action := range []string{"move", "copy"} {
		t.Run(action, func(t *testing.T) {
			e, f := setup(t, `require ["fileinto", "copy"]; fileinto :copy "Filed"; keep;`)
			backend := &portableBackend{fake: f}
			e.Remote = backend
			ref := f.add("INBOX", fixture, model.Flagged)
			cycle(t, e)
			before, err := state.List[model.Entry](e.State, "entry")
			if err != nil || len(before) != 2 || f.copies != 1 {
				t.Fatal("fixture did not finish initial filtering", before, err)
			}
			mappings := map[string]model.Ref{}
			for _, entry := range before {
				mappings[entry.ID] = entry.Remote
			}
			var ident identity
			if _, err := e.State.Get("meta", "identity", &ident); err != nil {
				t.Fatal(err)
			}
			// Older databases retain Root. It must be ignored even when it
			// names a path that does not exist on the replica's machine.
			legacy := map[string]string{"Owner": ident.Owner, "Server": ident.Server, "Username": ident.Username, "Root": "/unavailable-original-machine/mail"}
			if err := e.State.Put("meta", "identity", legacy); err != nil {
				t.Fatal(err)
			}
			original := e.Config
			source := e.Local.Root
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "relocated")
			if action == "move" {
				err = os.Rename(source, destination)
			} else {
				err = os.CopyFS(destination, os.DirFS(source))
			}
			if err != nil {
				t.Fatal(err)
			}
			c := original
			c.Maildir = destination
			// Read-only tooling must accept relocated stores too, without
			// editing the database to make them usable.
			view, err := View(c)
			if err != nil {
				t.Fatal("read-only open rejected relocated store", err)
			}
			var saved map[string]string
			if _, err := view.State.Get("meta", "identity", &saved); err != nil || !reflect.DeepEqual(saved, legacy) {
				t.Fatal("view rewrote legacy identity", saved, err)
			}
			view.Close()
			// Only one replica runs new Sieve decisions for the account.
			c.FilterFolders = nil
			replica, err := Open(c)
			if err != nil {
				t.Fatal("writable open rejected relocated store", err)
			}
			defer replica.Close()
			replica.Remote = backend
			fetches := backend.fetches
			cycle(t, replica)
			if backend.fetches != fetches || f.copies != 1 || len(scan(t, replica)) != 2 {
				t.Fatal("existing mail was downloaded or filtered again")
			}
			entries, err := state.List[model.Entry](replica.State, "entry")
			if err != nil || len(entries) != len(before) {
				t.Fatal("entry mappings lost", err)
			}
			for _, entry := range entries {
				if mappings[entry.ID] != entry.Remote {
					t.Fatal("entry identity or UID mapping changed", entry)
				}
			}
			if _, err := replica.State.Get("meta", "identity", &saved); err != nil || !reflect.DeepEqual(saved, legacy) {
				t.Fatal("opening relocated store required an identity rewrite", saved, err)
			}
			if other, err := Open(c); err == nil {
				other.Close()
				t.Fatal("relocated store accepted a second writer")
			}
			if action == "copy" {
				// Copies have independent locks; both can sync the account.
				first, err := Open(original)
				if err != nil {
					t.Fatal("replica prevented original writer from opening", err)
				}
				defer first.Close()
				first.Remote = backend
				if err := f.Flags(context.Background(), ref, model.Seen, 0); err != nil {
					t.Fatal(err)
				}
				cycle(t, replica)
				cycle(t, first)
				for _, store := range []*Engine{replica, first} {
					for _, local := range scan(t, store) {
						if local.Folder == "INBOX" && local.Flags&model.Seen == 0 {
							t.Fatal("independent replica did not catch up with flags")
						}
					}
				}
			}
			if backend.fetches != fetches || f.copies != 1 {
				t.Fatal("catch-up redownloaded or refiltered existing mail")
			}
			f.add("INBOX", []byte("From: new@example.org\r\nSubject: New\r\n\r\nnew body\r\n"), 0)
			cycle(t, replica)
			if backend.fetches != fetches+1 || f.copies != 1 || len(scan(t, replica)) != 3 {
				t.Fatal("replica did not download only the new membership")
			}
		})
	}
}

func TestPortableStoreStillRequiresAccountAndOwner(t *testing.T) {
	for _, mismatch := range []string{"server", "username", "owner"} {
		t.Run(mismatch, func(t *testing.T) {
			e, f := setup(t, "")
			f.add("INBOX", fixture, 0)
			cycle(t, e)
			c := e.Config
			e.Close()
			switch mismatch {
			case "server":
				c.Server = "other.example.org:993"
			case "username":
				c.Username = "other"
			case "owner":
				if err := os.WriteFile(filepath.Join(c.StateDir(), "owner"), []byte("wrong-owner"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if view, err := View(c); err == nil {
				view.Close()
				t.Fatal("view accepted mismatched " + mismatch)
			}
			if writer, err := Open(c); err == nil {
				writer.Close()
				t.Fatal("writer accepted mismatched " + mismatch)
			}
		})
	}
}
