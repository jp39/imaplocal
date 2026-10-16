package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
)

func below(name, root string) bool { return name == root || strings.HasPrefix(name, root+"/") }

func (e *Engine) folderIssue(id string, err error) error {
	if save := e.issue("folder:"+id, "folder_blocked", err.Error()); save != nil {
		return save
	}
	return err
}

// currentFolder follows a recorded local rename. A retired name without a
// successor is suppressed, so explicit config/rules cannot resurrect it.
func (e *Engine) currentFolder(name string) (string, error) {
	for range 100 {
		var record model.Folder
		found, err := e.State.Get("folder", name, &record)
		if err != nil {
			return "", err
		}
		if !found || !record.Retired {
			return name, nil
		}
		if record.ReplacedBy == "" {
			return "", nil
		}
		name = record.ReplacedBy
	}
	return "", errors.New("folder rename alias cycle")
}

// configuredFolder includes literal selectors following a recorded rename.
func (e *Engine) configuredFolder(name string) (bool, error) {
	if e.Config.SelectsFolder(name) {
		return true, nil
	}
	for _, pattern := range e.Config.Folders {
		if config.FolderPattern(pattern) {
			continue
		}
		current, err := e.currentFolder(pattern)
		if err != nil {
			return false, err
		}
		if current == name {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) trackFolder(name string) error {
	var record model.Folder
	found, err := e.State.Get("folder", name, &record)
	if err != nil {
		return err
	}
	locations, err := e.Local.FolderLocations()
	if err != nil {
		return err
	}
	if found && !record.Retired {
		if locations[record.ID] != name {
			return errors.New("local folder moved or disappeared during inventory")
		}
	} else {
		record = model.Folder{ID: model.NewID(), Name: name}
		for id, location := range locations {
			if location == name {
				record.ID = id
			}
		}
	}
	var boundary model.Boundary
	_, err = e.State.Get("boundary", name, &boundary)
	if err != nil {
		return err
	}
	var previous model.Folder
	hadPrevious, err := e.State.Get("folder", name, &previous)
	if err != nil {
		return err
	}
	if hadPrevious && previous.Retired && previous.Validity != 0 && previous.Validity == boundary.Validity {
		return errors.New("recreated folder reused its previous UIDVALIDITY; manual identity review required")
	}
	record.Validity = boundary.Validity
	if err = e.Local.TrackFolder(name, record.ID); err != nil {
		return err
	}
	return e.State.Put("folder", name, record)
}

// observeFolderChanges uses only complete LIST observations. It never infers
// remote identity from UIDVALIDITY or message hashes: these are not mailbox IDs.
func (e *Engine) observeFolderChanges(ctx context.Context) (map[string]bool, error) {
	lister, ok := e.Remote.(model.FolderLister)
	if !ok {
		return nil, nil
	}
	names, err := lister.ListFolders(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover remote folders: %w", err)
	}
	remote := map[string]bool{}
	for _, name := range names {
		if e.Config.SelectsFolder(name) {
			if err = config.Folder(name); err != nil {
				return nil, err
			}
		}
		remote[name] = true
	}
	if err = e.adoptLegacyFolders(remote); err != nil {
		return nil, err
	}
	operations, err := state.List[model.FolderOperation](e.State, "folder_operation")
	if err != nil {
		return nil, err
	}
	for _, op := range operations {
		if op.Phase != "done" {
			if err = e.resumeFolder(ctx, &op); err != nil {
				return nil, e.folderIssue(op.ID, err)
			}
			// Refresh after every namespace mutation before considering another change.
			return e.observeFolderChanges(ctx)
		}
	}
	records, err := state.List[model.Folder](e.State, "folder")
	if err != nil {
		return nil, err
	}
	locations, err := e.Local.FolderLocations()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		selected, err := e.configuredFolder(record.Name)
		if err != nil {
			return nil, err
		}
		if !selected {
			continue
		}
		if record.Retired {
			continue
		}
		location := locations[record.ID]
		if remote[record.Name] && location == record.Name {
			continue
		}
		if strings.EqualFold(record.Name, "INBOX") {
			return nil, e.folderIssue(record.ID, errors.New("INBOX missing or renamed; refusing special IMAP INBOX rename semantics"))
		}
		// Namespace changes may not race with unfinished message effects.
		pending, err := state.List[model.Operation](e.State, "operation")
		if err != nil {
			return nil, err
		}
		for _, op := range pending {
			if op.Phase != "done" || (op.ArrivalID == "" && !op.Applied) {
				return nil, e.folderIssue(record.ID, errors.New("finish or resolve message operations before changing folders"))
			}
		}
		arrivals, err := state.List[model.Arrival](e.State, "arrival")
		if err != nil {
			return nil, err
		}
		for _, a := range arrivals {
			if !a.Done {
				return nil, e.folderIssue(record.ID, errors.New("finish pending arrivals before changing folders"))
			}
		}
		op := model.FolderOperation{ID: model.NewID(), Source: record.Name, Phase: "planned"}
		if !remote[record.Name] {
			if location != "" && location != record.Name {
				return nil, e.folderIssue(record.ID, errors.New("local and remote folder names changed concurrently; manual resolution required"))
			}
			op.Kind = "retire"
			op.Mappings = []model.FolderMapping{{From: record.Name, ID: record.ID, Validity: record.Validity}}
		} else {
			if !e.Config.RemoteWrites {
				return nil, e.folderIssue(record.ID, errors.New("local folder renamed or removed; remote_writes is false"))
			}
			if _, ok = e.Remote.(model.FolderManager); !ok {
				return nil, e.folderIssue(record.ID, errors.New("backend cannot change folders"))
			}
			if location == "" {
				op.Kind = "remove"
				// IMAP DELETE is not recursive. Remove one deepest mailbox per
				// journal entry so a lost reply cannot replay a whole subtree.
				source := record.Name
				for name := range remote {
					if below(name, record.Name) && len(name) > len(source) {
						source = name
					}
				}
				id := model.NewID()
				var expected uint32
				for _, child := range records {
					if child.Name == source && !child.Retired {
						id = child.ID
						expected = child.Validity
						if locations[id] != "" {
							return nil, e.folderIssue(record.ID, errors.New("local parent removed but child survives; refusing recursive removal"))
						}
					}
				}
				box, err := e.Remote.Snapshot(ctx, source)
				if err != nil {
					return nil, err
				}
				if expected != 0 && box.Validity != expected {
					return nil, e.folderIssue(record.ID, errors.New("folder epoch changed before removal"))
				}
				op.Source = source
				op.Mappings = []model.FolderMapping{{From: source, ID: id, Validity: box.Validity, NextUID: box.NextUID}}
			} else {
				op.Kind = "rename"
				op.Destination = location
				if err = config.Folder(location); err != nil {
					return nil, err
				}
				if below(location, op.Source) || strings.EqualFold(location, "INBOX") {
					return nil, e.folderIssue(record.ID, errors.New("invalid local folder rename destination"))
				}
				for name := range remote {
					if below(name, location) {
						return nil, e.folderIssue(record.ID, errors.New("folder rename destination already exists remotely"))
					}
				}
				for _, name := range model.SortedFolders(remote) {
					if !below(name, record.Name) {
						continue
					}
					mapping := model.FolderMapping{From: name, To: location + strings.TrimPrefix(name, record.Name), ID: model.NewID()}
					var expected uint32
					for _, child := range records {
						if child.Name == name && !child.Retired {
							mapping.ID = child.ID
							expected = child.Validity
							if locations[child.ID] != mapping.To {
								return nil, e.folderIssue(record.ID, errors.New("inconsistent local subtree rename"))
							}
						}
					}
					box, err := e.Remote.Snapshot(ctx, name)
					if err != nil {
						return nil, err
					}
					if expected != 0 && box.Validity != expected {
						return nil, e.folderIssue(record.ID, errors.New("folder epoch changed before rename"))
					}
					mapping.Validity = box.Validity
					mapping.NextUID = box.NextUID
					op.Mappings = append(op.Mappings, mapping)
				}
			}
		}
		if err = e.State.Put("folder_operation", op.ID, op); err != nil {
			return nil, err
		}
		if err = e.resumeFolder(ctx, &op); err != nil {
			return nil, e.folderIssue(op.ID, err)
		}
		return e.observeFolderChanges(ctx)
	}
	return remote, nil
}

func (e *Engine) resumeFolder(ctx context.Context, op *model.FolderOperation) error {
	if op.Phase == "unknown" || op.Phase == "inflight" {
		return fmt.Errorf("folder operation %s has an unknown remote outcome; use resolve -folder", op.ID)
	}
	if op.Phase == "planned" {
		if op.Kind != "retire" {
			if !e.Config.RemoteWrites {
				return errors.New("folder mutation pending; remote_writes is false")
			}
			manager, ok := e.Remote.(model.FolderManager)
			if !ok {
				return errors.New("backend cannot rename folders")
			}
			// Preflight epochs on every planned attempt, including an explicitly
			// authorized retry. Never delete a recreated mailbox at an old name.
			for _, mapping := range op.Mappings {
				box, err := e.Remote.Snapshot(ctx, mapping.From)
				if err != nil {
					return err
				}
				if box.Validity != mapping.Validity {
					return errors.New("folder epoch changed before mutation")
				}
			}
			op.Phase = "inflight"
			if err := e.State.Put("folder_operation", op.ID, *op); err != nil {
				return err
			}
			var err error
			if op.Kind == "rename" {
				err = manager.RenameFolder(ctx, op.Source, op.Destination)
			} else {
				err = manager.DeleteFolder(ctx, op.Source)
			}
			if err != nil {
				op.Phase = "unknown"
				var rejected *model.FolderRejectedError
				if errors.As(err, &rejected) {
					op.Phase = "planned"
				}
				op.Error = err.Error()
				if save := e.State.Put("folder_operation", op.ID, *op); save != nil {
					return save
				}
				return err
			}
		}
		op.Phase = "remote_done"
		op.Error = ""
		if err := e.State.Put("folder_operation", op.ID, *op); err != nil {
			return err
		}
	}
	if op.Phase != "remote_done" {
		return fmt.Errorf("unexpected folder operation phase %q", op.Phase)
	}
	if op.Kind != "retire" {
		lister, ok := e.Remote.(model.FolderLister)
		if !ok {
			return errors.New("backend cannot verify folder outcome")
		}
		names, err := lister.ListFolders(ctx)
		if err != nil {
			return err
		}
		present := map[string]bool{}
		for _, name := range names {
			present[name] = true
		}
		for _, m := range op.Mappings {
			if present[m.From] || (op.Kind == "rename" && !present[m.To]) {
				return errors.New("folder rename outcome does not match the recorded namespace; manual resolution required")
			}
		}
	}
	entries, err := state.List[model.Entry](e.State, "entry")
	if err != nil {
		return err
	}
	changes := []state.Change{}
	for _, mapping := range op.Mappings {
		if op.Kind == "rename" {
			box, err := e.Remote.Snapshot(ctx, mapping.To)
			if err != nil {
				return err
			}
			if box.Validity != mapping.Validity {
				return errors.New("UIDVALIDITY changed across folder rename; mapping needs manual recovery")
			}
			// Validate memberships before rebinding. Equal UIDVALIDITY in two distinct
			// mailboxes is not proof of identity; a completed RENAME is the provenance.
			present := map[uint32]bool{}
			for _, m := range box.Messages {
				present[m.Ref.UID] = true
			}
			for i := range entries {
				entry := &entries[i]
				if entry.Remote.Folder != mapping.From || entry.Tombstone {
					continue
				}
				if entry.Remote.Validity != box.Validity {
					return errors.New("tracked folder epoch disagrees with renamed folder")
				}
				if present[entry.Remote.UID] {
					r := entry.Remote
					r.Folder = mapping.To
					raw, err := e.Remote.Fetch(ctx, r, e.Config.MaxMessageBytes)
					if err != nil {
						return err
					}
					if maildir.Hash(raw) != entry.Hash {
						return errors.New("renamed membership bytes do not match saved identity")
					}
				}
				entry.Remote.Folder = mapping.To
				changes = append(changes, state.Change{Kind: "entry", ID: entry.ID, Value: *entry})
			}
			var boundary model.Boundary
			found, err := e.State.Get("boundary", mapping.From, &boundary)
			if err != nil {
				return err
			}
			if !found {
				boundary = model.Boundary{Validity: mapping.Validity, NextUID: mapping.NextUID}
			}
			changes = append(changes, state.Change{Kind: "boundary", ID: mapping.To, Value: boundary}, state.Change{Kind: "folder", ID: mapping.To, Value: model.Folder{ID: mapping.ID, Name: mapping.To, Validity: mapping.Validity}})
			// Existing markers must have moved with the local rename. Only previously
			// untracked remote children get a new local directory/marker.
			if err = e.Local.TrackFolder(mapping.To, mapping.ID); err != nil {
				return err
			}
		} else {
			// No archive: delete only content still equal to the last agreed
			// state. Pending local edits and unmapped drafts are conflicts.
			observed, err := e.removableFolder(mapping.From, entries)
			if err != nil {
				return err
			}
			if err = e.Local.RemoveFolderCore(mapping.From, observed); err != nil {
				return err
			}
			for i := range entries {
				if entries[i].Remote.Folder == mapping.From {
					entries[i].Tombstone = true
					entries[i].FolderRemoved = true
					changes = append(changes, state.Change{Kind: "entry", ID: entries[i].ID, Value: entries[i]})
				}
			}
		}
		successor := ""
		if op.Kind == "rename" {
			successor = mapping.To
		}
		changes = append(changes, state.Change{Kind: "folder", ID: mapping.From, Value: model.Folder{ID: mapping.ID, Name: mapping.From, Validity: mapping.Validity, Retired: true, ReplacedBy: successor}}, state.Change{Kind: "boundary", ID: mapping.From, Delete: true})
	}
	op.Phase = "done"
	op.Error = ""
	changes = append(changes, state.Change{Kind: "folder_operation", ID: op.ID, Value: *op})
	return e.State.Apply(changes)
}

// ResolveFolder records an operator-verified outcome. The next sync validates
// the destination epoch and bytes before rebinding a supplied applied outcome.
func ResolveFolder(st *state.Store, id string, applied bool) error {
	var op model.FolderOperation
	found, err := st.Get("folder_operation", id, &op)
	if err != nil {
		return err
	}
	if !found || op.Phase != "unknown" {
		return errors.New("folder operation is not an unresolved unknown outcome")
	}
	if applied {
		op.Phase = "remote_done"
	} else {
		op.Phase = "planned"
	}
	op.Error = ""
	return st.Put("folder_operation", op.ID, op)
}

func (e *Engine) removableFolder(name string, entries []model.Entry) ([]model.Local, error) {
	locals, err := e.Local.Scan()
	if err != nil {
		return nil, err
	}
	known := map[string]model.Entry{}
	for _, entry := range entries {
		if !entry.FolderRemoved && entry.Remote.Folder == name {
			known[entry.ID] = entry
		}
	}
	observed := []model.Local{}
	for _, local := range locals {
		if local.Folder == name {
			entry, ok := known[local.ID]
			if !ok || local.Flags != entry.Agreed {
				return nil, errors.New("folder removal conflicts with pending local messages or flag changes")
			}
			raw, err := maildir.Read(local, e.Config.MaxMessageBytes)
			if err != nil {
				return nil, err
			}
			if maildir.Hash(raw) != entry.Hash {
				return nil, errors.New("folder removal conflicts with an edited local message")
			}
			observed = append(observed, local)
		}
	}
	return observed, nil
}

// Older alpha stores have boundaries and entries but no folder markers. Adopt
// those identities before interpreting disappearance; never turn an upgrade's
// missing local directory into permission to delete an existing remote mailbox.
func (e *Engine) adoptLegacyFolders(remote map[string]bool) error {
	names, err := e.State.Keys("boundary")
	if err != nil {
		return err
	}
	for _, name := range names {
		selected, err := e.configuredFolder(name)
		if err != nil {
			return err
		}
		if !selected {
			continue
		}
		var record model.Folder
		found, err := e.State.Get("folder", name, &record)
		if err != nil {
			return err
		}
		if found {
			continue
		}
		var boundary model.Boundary
		if _, err = e.State.Get("boundary", name, &boundary); err != nil {
			return err
		}
		record = model.Folder{ID: model.NewID(), Name: name, Validity: boundary.Validity}
		folder, err := e.Local.Folder(name)
		if err != nil {
			return err
		}
		if _, err = os.Lstat(filepath.Join(folder, "cur")); err == nil {
			// A marker may already exist after a crash between marker and state writes.
			locations, err := e.Local.FolderLocations()
			if err != nil {
				return err
			}
			for id, location := range locations {
				if location == name {
					record.ID = id
				}
			}
			if err = e.Local.TrackFolder(name, record.ID); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		} else if remote[name] {
			return fmt.Errorf("legacy local folder %q is missing; restore it before enabling folder synchronization", name)
		}
		if err = e.State.Put("folder", name, record); err != nil {
			return err
		}
	}
	return nil
}
