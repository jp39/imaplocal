package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/sieve"
	"github.com/jp39/imaplocal/internal/state"
	"github.com/jp39/imaplocal/internal/status"
)

type Engine struct {
	Log *slog.Logger
	// Progress reports work completed without changing reconciliation decisions.
	Progress func(phase, folder string, completed, total int)
	// SelectedFolders is the latest successful explicit/discovered folder selection.
	SelectedFolders []string
	Config          config.Config
	State           *state.Store
	Local           *maildir.Store
	Filter          *sieve.Filter
	Remote          model.Backend
}
type identity struct {
	Owner    string
	Server   string
	Username string
}

func Open(c config.Config) (*Engine, error) { return open(c, false) }

// View never acquires the writer lock or recovers/mutates pending operations.
func View(c config.Config) (*Engine, error) { return open(c, true) }

func open(c config.Config, readOnly bool) (*Engine, error) {
	// Resolve symlinked ancestors so state and Maildir observations use the
	// same path, including when descendants do not exist yet.
	canonical := func(p string) (string, error) {
		tail := []string{}
		for {
			r, e := filepath.EvalSymlinks(p)
			if e == nil {
				for i := len(tail) - 1; i >= 0; i-- {
					r = filepath.Join(r, tail[i])
				}
				return r, nil
			}
			if !os.IsNotExist(e) {
				return "", e
			}
			parent := filepath.Dir(p)
			if parent == p {
				return "", e
			}
			tail = append(tail, filepath.Base(p))
			p = parent
		}
	}
	var err error
	c.Maildir, err = canonical(c.Maildir)
	if err != nil {
		return nil, err
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	filter, err := sieve.Load(c.Sieve)
	if err != nil {
		return nil, err
	}
	st, err := state.Open(c.StateDir(), readOnly)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*Engine, error) { st.Close(); return nil, e }
	var ident identity
	found, err := st.Get("meta", "identity", &ident)
	if err != nil {
		return fail(err)
	}
	if !found {
		if readOnly {
			return fail(errors.New("store identity not initialized"))
		}
		ident = identity{Owner: model.NewID(), Server: c.Server, Username: c.Username}
		if err = st.Put("meta", "identity", ident); err != nil {
			return fail(err)
		}
	} else if ident.Server != c.Server || ident.Username != c.Username {
		return fail(errors.New("state directory is bound to a different account"))
	}
	var local *maildir.Store
	if readOnly {
		local, err = maildir.Inspect(c.Maildir, c.StateDir(), ident.Owner)
	} else {
		local, err = maildir.New(c.Maildir, c.StateDir(), ident.Owner)
	}
	if err != nil {
		return fail(err)
	}
	if readOnly {
		return &Engine{Config: c, State: st, Local: local, Filter: filter}, nil
	}
	ops, err := state.List[model.Operation](st, "operation")
	if err != nil {
		return fail(err)
	}
	for _, op := range ops {
		if op.Phase == "inflight" {
			op.Phase = "unknown"
			op.Error = "process stopped while a remote mutation may have been in flight"
			if err = st.Put("operation", op.ID, op); err != nil {
				return fail(err)
			}
		}
	}
	folderOps, err := state.List[model.FolderOperation](st, "folder_operation")
	if err != nil {
		return fail(err)
	}
	for _, op := range folderOps {
		if op.Phase == "inflight" {
			op.Phase = "unknown"
			op.Error = "process stopped while folder mutation may have been in flight"
			if err = st.Put("folder_operation", op.ID, op); err != nil {
				return fail(err)
			}
		}
	}
	return &Engine{Config: c, State: st, Local: local, Filter: filter}, nil
}
func (e *Engine) Close() error { return e.State.Close() }
func (e *Engine) issue(id, kind, detail string) error {
	return e.State.Put("issue", id, model.Issue{ID: id, Kind: kind, Detail: detail, Updated: time.Now().UTC()})
}
func group(index []model.Local) map[string][]model.Local {
	m := map[string][]model.Local{}
	for _, l := range index {
		if l.ID != "" {
			m[l.ID] = append(m[l.ID], l)
		}
	}
	return m
}

// Cycle reconciles complete observations. It does not interpret a failed scan
// or failed IMAP inventory as an empty mailbox.
func (e *Engine) Cycle(ctx context.Context) error {
	if e.Remote == nil {
		return errors.New("no remote backend")
	}
	index, err := e.Local.Scan()
	if err != nil {
		return err
	}
	entries, err := state.List[model.Entry](e.State, "entry")
	if err != nil {
		return err
	}
	arrivals, err := state.List[model.Arrival](e.State, "arrival")
	if err != nil {
		return err
	}
	ops, err := state.List[model.Operation](e.State, "operation")
	if err != nil {
		return err
	}
	for _, a := range arrivals {
		if a.Done && !a.Cleaned {
			if err = e.cleanArrival(&a); err != nil {
				return err
			}
		}
	}
	for _, op := range ops {
		if op.ArrivalID == "" && op.Applied && !op.Cleaned {
			if err = e.cleanOperation(&op); err != nil {
				return err
			}
		}
	}
	observed, err := e.observeFolderChanges(ctx)
	if err != nil {
		return err
	}
	// Folder recovery may atomically rebind or retire message memberships.
	entries, err = state.List[model.Entry](e.State, "entry")
	if err != nil {
		return err
	}
	index, err = e.Local.Scan()
	if err != nil {
		return err
	}
	folders := map[string]bool{}
	discoveredFolders := map[string]bool{}
	if e.Config.NeedsDiscovery() {
		if observed == nil {
			return errors.New("remote backend does not support folder discovery")
		}
		for name := range observed {
			if e.Config.SelectsFolder(name) {
				folders[name] = true
				discoveredFolders[name] = true
			}
		}
	}
	for _, f := range e.Config.Folders {
		if config.FolderPattern(f) {
			continue
		}
		current, err := e.currentFolder(f)
		if err != nil {
			return err
		}
		// An explicitly selected name may be recreated as a fresh mailbox.
		// Its new epoch is checked while tracking; absence never triggers CREATE.
		if current == "" && observed[f] {
			current = f
		}
		if current != "" {
			folders[current] = true
		}
	}

	e.SelectedFolders = model.SortedFolders(folders)

	// Changing selectors pauses unrelated tracked folders; saved mappings and
	// local files must not silently re-enable them or imply remote disappearance.
	activeIDs := map[string]bool{}
	for _, entry := range entries {
		if folders[entry.Remote.Folder] && !entry.Tombstone {
			activeIDs[entry.ID] = true
		}
	}
	for _, a := range arrivals {
		if a.Done {
			continue
		}
		folders[a.Source.Ref.Folder] = true
		for _, d := range a.Deliveries {
			folders[d.Folder] = true
		}
	}
	for _, l := range index {
		if activeIDs[l.ID] || e.Config.SelectsFolder(l.Folder) {
			folders[l.Folder] = true
		}
	}
	// Destinations left by an interrupted local transfer also need inventory.
	for _, op := range ops {
		if op.Phase == "done" && (op.ArrivalID != "" || op.Applied) {
			continue
		}
		if op.Source.Folder != "" {
			folders[op.Source.Folder] = true
		}
		if op.Destination != "" {
			folders[op.Destination] = true
		}
	}
	boxes := map[string]model.Mailbox{}
	remote := map[string]model.Message{}
	folderNames := model.SortedFolders(folders)
	for i, f := range folderNames {
		var b model.Boundary
		found, err := e.State.Get("boundary", f, &b)
		if err != nil {
			return err
		}
		// LIST already proved existence. If it disappears before SELECT, fail;
		// discovery must not recreate a mailbox deleted concurrently.
		if !found && !discoveredFolders[f] {
			if err = e.Remote.Ensure(ctx, f); err != nil {
				return fmt.Errorf("mailbox %q: %w", f, err)
			}
		}
		box, err := e.inventory(ctx, f)
		if err != nil {
			return fmt.Errorf("inventory %q: %w", f, err)
		}
		if !found {
			b = model.Boundary{Validity: box.Validity, NextUID: box.NextUID}
			if err = e.State.Put("boundary", f, b); err != nil {
				return err
			}
		} else if b.Validity != box.Validity {
			_ = e.issue("epoch:"+f, "uidvalidity_changed", fmt.Sprintf("%s changed UIDVALIDITY from %d to %d; state adoption requires operator review", f, b.Validity, box.Validity))
			return errors.New("UIDVALIDITY changed; account reconciliation stopped")
		}
		// Empty remote mailboxes should also be visible to Maildir clients.
		if observed != nil {
			if err = e.trackFolder(f); err != nil {
				return err
			}
		} else if err = e.Local.Ensure(f); err != nil {
			return err
		}
		if e.Progress != nil {
			e.Progress("inventory", f, i+1, len(folderNames))
		}
		boxes[f] = box
		for _, m := range box.Messages {
			remote[m.Ref.Key()] = m
		}
	}
	// Recompute current issues from observations; operation history is never reset.
	issues, err := state.List[model.Issue](e.State, "issue")
	if err != nil {
		return err
	}
	for _, i := range issues {
		if err = e.State.Delete("issue", i.ID); err != nil {
			return err
		}
	}
	// First finish journaled external work. Its stable IDs reserve local files
	// even if the prior process died between a rename and saving its mapping.
	for _, op := range ops {
		if op.ArrivalID == "" && !op.Applied {
			if op.Kind == "delete" {
				if err = e.resumeDelete(ctx, &op); err != nil {
					if er := e.issue(op.ID, "delete_blocked", err.Error()); er != nil {
						return er
					}
				} else if op.Applied {
					// Reinventory: cancellation leaves the source alive.
					box, er := e.Remote.Snapshot(ctx, op.Source.Folder)
					if er != nil {
						return er
					}
					delete(remote, op.Source.Key())
					for _, m := range box.Messages {
						remote[m.Ref.Key()] = m
					}
				}
				continue
			}
			if err = e.resumeExternal(ctx, &op, index); err != nil {
				if er := e.issue(op.ID, "operation_blocked", err.Error()); er != nil {
					return er
				}
			} else {
				// The inventory predates our resumed operation. Its result
				// must supersede that snapshot before ordinary reconciliation.
				current, fetchErr := e.Remote.Metadata(ctx, op.Result)
				if fetchErr != nil {
					return fetchErr
				}
				remote[op.Result.Key()] = current
				if op.Kind == "move" {
					delete(remote, op.Source.Key())
				}
			}
		}
	}
	index, err = e.Local.Scan()
	if err != nil {
		return err
	}
	entries, err = state.List[model.Entry](e.State, "entry")
	if err != nil {
		return err
	}
	pendingOps, err := state.List[model.Operation](e.State, "operation")
	if err != nil {
		return err
	}
	blockedIDs := map[string]bool{}
	// A manual filter owns its original mapping until all deliveries and
	// source cleanup complete, including after a lost MOVE acknowledgement.
	for _, a := range arrivals {
		if !a.Done && a.OriginID != "" {
			blockedIDs[a.OriginID] = true
		}
	}
	for _, op := range pendingOps {
		if !op.Applied && op.ArrivalID == "" {
			blockedIDs[op.EntryID] = true
			blockedIDs[op.OriginID] = true
		}
	}
	byID := group(index)
	known := map[string]model.Entry{}
	for _, entry := range entries {
		known[entry.ID] = entry
	}
	for _, entry := range entries {
		if !folders[entry.Remote.Folder] {
			continue
		}
		if entry.FolderRemoved {
			continue
		}
		if blockedIDs[entry.ID] {
			continue
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.Remote.UID == 0 {
			continue
		} // upload is journaled, never rediscovered
		locals := byID[entry.ID]
		m, present := remote[entry.Remote.Key()]
		if entry.Tombstone || !present {
			if err = e.remoteRemoval(entry, locals); err != nil {
				return err
			}
			continue
		}
		if len(locals) == 0 || (len(locals) == 1 && locals[0].Deleted) {
			if err = e.planDelete(ctx, entry, m, locals); err != nil {
				if er := e.issue(entry.ID, "delete_blocked", err.Error()); er != nil {
					return er
				}
			}
			continue
		}
		deleting := false
		for _, l := range locals {
			deleting = deleting || l.Deleted
		}
		if deleting {
			if err = e.issue(entry.ID, "delete_conflict", "deletion conflicts with local copies or moves"); err != nil {
				return err
			}
			continue
		}
		var home *model.Local
		for i := range locals {
			if locals[i].Folder == entry.Remote.Folder {
				if home != nil {
					home = nil
					break
				}
				home = &locals[i]
			}
		}
		if len(locals) > 1 && home == nil {
			if err = e.issue(entry.ID, "ambiguous_move", "multiple local copies without the original membership; no remote changes made"); err != nil {
				return err
			}
			continue
		}
		if home == nil {
			l := locals[0]
			if err = e.localTransfer(ctx, entry, m, l, true, index); err != nil {
				if er := e.issue(entry.ID, "local_move", err.Error()); er != nil {
					return er
				}
			}
			continue
		}
		rawNeeded := home.Size != entry.Size || home.ModTime != entry.ModTime
		if rawNeeded {
			raw, err := maildir.Read(*home, e.Config.MaxMessageBytes)
			if err != nil {
				return err
			}
			if maildir.Hash(raw) != entry.Hash {
				if err = e.issue(entry.ID, "body_changed", "local message body was edited; preserved for manual conflict resolution"); err != nil {
					return err
				}
				continue
			}
			entry.Size = home.Size
			entry.ModTime = home.ModTime
		}
		merged := MergeFlags(entry.Agreed, home.Flags, m.Flags)
		if e.Log != nil && (home.Flags != entry.Agreed || m.Flags != entry.Agreed) {
			e.Log.Info("message flags changed", "folder", entry.Remote.Folder, "uidvalidity", entry.Remote.Validity, "uid", entry.Remote.UID, "filename", home.Path, "previous", entry.Agreed.Maildir(), "local", home.Flags.Maildir(), "remote", m.Flags.Maildir(), "merged", merged.Maildir())
		}
		if merged != m.Flags {
			if !e.Config.RemoteWrites {
				if err = e.issue(entry.ID, "writes_disabled", "local flags differ; remote_writes is false"); err != nil {
					return err
				}
				continue
			}
			if err = e.Remote.Flags(ctx, entry.Remote, merged&^m.Flags, m.Flags&^merged); err != nil {
				return err
			}
		}
		if home.Flags != merged {
			updated, err := e.Local.Rename(*home, entry.ID, merged)
			if err != nil {
				return err
			}
			*home = updated
		}
		entry.ReceivedAt = m.Date
		entry.Agreed = merged
		entry.Size = home.Size
		entry.ModTime = home.ModTime
		if err = e.State.Put("entry", entry.ID, entry); err != nil {
			return err
		}
		for _, l := range locals {
			if l.Folder != entry.Remote.Folder {
				if err = e.localTransfer(ctx, entry, m, l, false, index); err != nil {
					if er := e.issue(entry.ID+":"+l.Folder, "local_copy", err.Error()); er != nil {
						return er
					}
				}
			}
		}
	}
	// Adopt externally added cur/ entries only in this exclusively owned tree.
	// new/ remains the MUA handoff and is never consumed by this path.
	ops, err = state.List[model.Operation](e.State, "operation")
	if err != nil {
		return err
	}
	reserved := map[string]bool{}
	for _, op := range ops {
		if !op.Applied {
			reserved[op.EntryID] = true
			if op.OriginID != "" {
				reserved[op.OriginID] = true
			}
		}
	}
	for _, a := range arrivals {
		for _, d := range a.Deliveries {
			reserved[d.ID] = true
		}
	}
	for _, l := range index {
		if !folders[l.Folder] {
			continue
		}
		if _, ok := known[l.ID]; ok {
			continue
		}
		if l.Deleted || reserved[l.ID] || filepath.Base(filepath.Dir(l.Path)) != "cur" {
			continue
		}
		if !e.Config.RemoteWrites {
			if err = e.issue("new:"+l.Folder+":"+l.Key, "writes_disabled", "unmapped local message retained; remote_writes is false"); err != nil {
				return err
			}
			continue
		}
		if err = e.upload(ctx, l, index); err != nil {
			if er := e.issue("upload:"+l.Key, "upload_blocked", err.Error()); er != nil {
				return er
			}
		}
	}
	// Reload mappings after transfers before deciding which remote UIDs are new.
	entries, err = state.List[model.Entry](e.State, "entry")
	if err != nil {
		return err
	}
	mapped := map[string]bool{}
	for _, entry := range entries {
		mapped[entry.Remote.Key()] = true
	}
	ops, err = state.List[model.Operation](e.State, "operation")
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Kind == "move" {
			mapped[op.Source.Key()] = true
		}
		if op.Result.Valid() {
			mapped[op.Result.Key()] = true
		}
	}
	uncertainFolders := map[string]bool{}
	for _, op := range ops {
		if op.Kind != "delete" && (op.Phase == "unknown" || op.Phase == "inflight") {
			uncertainFolders[op.Destination] = true
		}
	}
	index, err = e.Local.Scan()
	if err != nil {
		return err
	}
	publicationIndex := group(index)
	for _, a := range arrivals {
		if !a.Done {
			if err = e.arrive(ctx, &a, publicationIndex); err != nil {
				if er := e.issue(a.ID, "arrival_blocked", err.Error()); er != nil {
					return er
				}
			}
		}
	}
	return e.downloadArrivals(ctx, remote, mapped, uncertainFolders, publicationIndex, arrivals)
}

func (e *Engine) runOperation(ctx context.Context, op *model.Operation, raw []byte) error {
	var saved model.Operation
	found, err := e.State.Get("operation", op.ID, &saved)
	if err != nil {
		return err
	}
	if found {
		*op = saved
	} else {
		op.Phase = "planned"
		if err = e.State.Put("operation", op.ID, op); err != nil {
			return err
		}
	}
	uncertain := op.Phase == "unknown" || op.Phase == "inflight"
	if uncertain {
		if op.Kind != "move" || !op.Source.Valid() {
			return fmt.Errorf("operation %s has an unknown outcome; inspect and resolve it explicitly", op.ID)
		}
		// A native MOVE consumes the original UID. Check that exact membership
		// afresh, not the cycle's inventory or Message-ID/body lookalikes. The
		// adapter validates UIDVALIDITY here and again when sending UID MOVE.
		// An absent source cannot establish the destination mapping, so it
		// remains uncertain. COPY and APPEND cannot use this retry policy.
		source, checkErr := e.Remote.Metadata(ctx, op.Source)
		if checkErr != nil {
			return fmt.Errorf("operation %s remains uncertain; cannot verify MOVE source: %w", op.ID, checkErr)
		}
		if source.Ref != op.Source {
			return fmt.Errorf("operation %s remains uncertain; MOVE source identity mismatch", op.ID)
		}
	}
	if op.Phase == "done" {
		return nil
	}
	if !e.Config.RemoteWrites {
		return errors.New("remote_writes is false")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if checker, ok := e.Remote.(interface {
		CheckOperation(context.Context, model.Operation) error
	}); ok {
		if err = checker.CheckOperation(ctx, *op); err != nil {
			return err
		}
	}
	if err = e.ensureDestination(ctx, op.Destination); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if uncertain {
		e.messageEvent("retrying uncertain MOVE; original source still exists", op.Source, raw,
			"destination", op.Destination, "operation_id", op.ID)
		op.Result = model.Ref{}
		op.Verified = false
		op.Error = ""
	}
	op.Phase = "inflight"
	if err = e.State.Put("operation", op.ID, op); err != nil {
		return err
	}
	e.messageEvent("remote message operation started", op.Source, raw, "operation", op.Kind, "destination", op.Destination, "operation_id", op.ID)
	switch op.Kind {
	case "append":
		op.Result, err = e.Remote.Append(ctx, op.Destination, raw, op.Flags, op.Date)
	case "copy", "move":
		op.Result, err = e.Remote.Transfer(ctx, op.Source, op.Destination, op.Kind == "move")
	default:
		err = errors.New("unsupported remote operation")
	}
	if err != nil {
		op.Phase = "unknown"
		op.Error = "remote mutation failed or its acknowledgement was lost"
		if saveErr := e.State.Put("operation", op.ID, op); saveErr != nil {
			return saveErr
		}
		return fmt.Errorf("operation %s needs inspection: %w", op.ID, err)
	}
	if !op.Result.Valid() || op.Result.Folder != op.Destination {
		return errors.New("invalid remote result; operation remains inflight for recovery")
	}
	var boundary model.Boundary
	if found, saveErr := e.State.Get("boundary", op.Destination, &boundary); saveErr != nil {
		return saveErr
	} else if found && boundary.Validity != op.Result.Validity {
		return errors.New("destination epoch changed during mutation; outcome needs review")
	}
	e.messageEvent("remote message operation completed", op.Result, raw, "operation", op.Kind, "operation_id", op.ID)
	op.Phase = "done"
	op.Verified = true
	op.Error = ""
	return e.State.Put("operation", op.ID, op)
}
func (e *Engine) verify(ctx context.Context, op *model.Operation) error {
	if op.Verified {
		return nil
	}
	raw, err := e.Remote.Fetch(ctx, op.Result, e.Config.MaxMessageBytes)
	if err != nil {
		return err
	}
	if maildir.Hash(raw) != op.Hash {
		return errors.New("resolved remote UID does not match operation content")
	}
	op.Verified = true
	return e.State.Put("operation", op.ID, op)
}
func (e *Engine) arrive(ctx context.Context, a *model.Arrival, index map[string][]model.Local) error {
	raw, err := e.Local.Staged(a.ID)
	if err != nil {
		return err
	}
	if maildir.Hash(raw) != a.Hash {
		return errors.New("staged arrival content changed")
	}
	if a.OriginID != "" {
		if err = e.checkFilterOrigin(a, index); err != nil {
			return err
		}
	}
	keep := false
	for _, d := range a.Deliveries {
		if d.Folder == a.Source.Ref.Folder {
			keep = true
		}
	}
	// Do copies before a final move, so the source remains valid until all
	// requested destinations have known remote memberships.
	order := []int{}
	for i, d := range a.Deliveries {
		if d.Folder != a.Source.Ref.Folder {
			order = append(order, i)
		}
	}
	for n, i := range order {
		d := &a.Deliveries[i]
		if d.Remote.Valid() {
			continue
		}
		kind := "copy"
		if !keep && n == len(order)-1 {
			kind = "move"
		}
		op := model.Operation{ID: a.ID + fmt.Sprintf(":%d", i), ArrivalID: a.ID, Kind: kind, Source: a.Source.Ref, Destination: d.Folder, EntryID: d.ID, Hash: a.Hash, Flags: d.Flags}
		if err = e.runOperation(ctx, &op, raw); err != nil {
			return err
		}
		if err = e.verify(ctx, &op); err != nil {
			return err
		}
		d.Remote = op.Result
		if err = e.State.Put("arrival", a.Source.Ref.Key(), a); err != nil {
			return err
		}
	}
	for i := range a.Deliveries {
		d := &a.Deliveries[i]
		var existing model.Entry
		completed, readErr := e.State.Get("entry", d.ID, &existing)
		if readErr != nil {
			return readErr
		}
		if completed {
			if existing.Remote != d.Remote && d.Remote.Valid() {
				return errors.New("delivery mapping changed during recovery")
			}
			// This delivery was already committed. Do not override later MUA
			// flags, edits, or deletion while completing another destination.
			continue
		}
		if !d.Remote.Valid() {
			d.Remote = a.Source.Ref
		}
		if d.Flags != a.Source.Flags {
			if !e.Config.RemoteWrites {
				return errors.New("filter changes flags but remote_writes is false")
			}
			if err = e.setFlags(ctx, d.Remote, d.Flags); err != nil {
				return err
			}
		}
		local, err := e.Local.Publish(*d, raw, index)
		if err != nil {
			return err
		}
		e.messageEvent("message published to Maildir", d.Remote, raw, "filename", local.Path, "flags", d.Flags.Maildir())
		index[local.ID] = []model.Local{local}
		entry := model.Entry{ReceivedAt: d.Date, ID: d.ID, Remote: d.Remote, Agreed: d.Flags, Hash: a.Hash, Size: local.Size, ModTime: local.ModTime}
		if err = e.State.Put("entry", entry.ID, entry); err != nil {
			return err
		}
	}
	if a.OriginID != "" {
		var origin model.Entry
		found, err := e.State.Get("entry", a.OriginID, &origin)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("manual filter source mapping missing")
		}
		if err = e.remoteRemoval(origin, index[a.OriginID]); err != nil {
			return err
		}
		delete(index, a.OriginID)
	}
	a.Done = true
	if err = e.State.Put("arrival", a.Source.Ref.Key(), a); err != nil {
		return err
	}
	return e.cleanArrival(a)
}
func (e *Engine) upload(ctx context.Context, l model.Local, index []model.Local) error {
	raw, err := maildir.Read(l, e.Config.MaxMessageBytes)
	if err != nil {
		return err
	}
	id := l.ID
	if id == "" {
		id = model.NewID()
	}
	// Record adoption before renaming; restart can find the old key via OriginID.
	op := model.Operation{ID: "upload:" + maildir.Hash([]byte(l.Folder+"\x00"+l.Key)), Kind: "append", LocalFlags: l.Flags, RemoteFlags: l.Flags, Destination: l.Folder, EntryID: id, OriginID: l.Key, Hash: maildir.Hash(raw), Flags: l.Flags, Date: time.Unix(0, l.ModTime)}
	var saved model.Operation
	found, err := e.State.Get("operation", op.ID, &saved)
	if err != nil {
		return err
	}
	if found {
		return e.resumeExternal(ctx, &saved, index)
	}
	if err = e.Local.Stage(op.IDSafe(), raw); err != nil {
		return err
	}
	if err = e.State.Put("operation", op.ID, op); err != nil {
		return err
	}
	return e.resumeExternal(ctx, &op, index)
}
func (e *Engine) localTransfer(ctx context.Context, entry model.Entry, remote model.Message, l model.Local, move bool, index []model.Local) error {
	if !e.Config.RemoteWrites {
		return errors.New("local move/copy retained; remote_writes is false")
	}
	raw, err := maildir.Read(l, e.Config.MaxMessageBytes)
	if err != nil {
		return err
	}
	if maildir.Hash(raw) != entry.Hash {
		return errors.New("local content changed; refusing to correlate by filename alone")
	}
	e.messageEvent("local message transfer detected", entry.Remote, raw, "filename", l.Path, "destination", l.Folder, "move", move)
	kind := "copy"
	id := model.NewID()
	if move {
		kind = "move"
		id = entry.ID
	}
	op := model.Operation{ID: kind + ":" + model.NewID(), Kind: kind, LocalFlags: l.Flags, RemoteFlags: remote.Flags, Source: entry.Remote, Destination: l.Folder, EntryID: id, OriginID: entry.ID, Hash: entry.Hash, Flags: MergeFlags(entry.Agreed, l.Flags, remote.Flags)}
	pending, err := state.List[model.Operation](e.State, "operation")
	if err != nil {
		return err
	}
	found := false
	for _, candidate := range pending {
		if !candidate.Applied && candidate.ArrivalID == "" && candidate.OriginID == entry.ID && candidate.Destination == l.Folder {
			op = candidate
			found = true
			break
		}
	}
	if err = e.Local.Stage(op.IDSafe(), raw); err != nil {
		return err
	}
	if !found {
		if err = e.State.Put("operation", op.ID, op); err != nil {
			return err
		}
	}
	return e.resumeExternal(ctx, &op, index)
}
func (e *Engine) resumeExternal(ctx context.Context, op *model.Operation, index []model.Local) error {
	raw, err := e.Local.Staged(op.IDSafe())
	if err != nil {
		return err
	}
	if maildir.Hash(raw) != op.Hash {
		return errors.New("staged operation content changed")
	}
	// Do not send a planned request if the local intent changed in the meantime.
	var local *model.Local
	for i := range index {
		l := &index[i]
		if l.Folder != op.Destination {
			continue
		}
		if l.ID == op.EntryID || (op.Kind == "append" && l.Key == op.OriginID) || (op.Kind != "append" && l.ID == op.OriginID) {
			if local != nil {
				return errors.New("ambiguous local operation target")
			}
			local = l
		}
	}
	if local == nil {
		return errors.New("local operation target missing or moved again; remote operation paused")
	}
	if local.Deleted {
		return errors.New("local operation target marked for deletion; operation paused")
	}
	data, err := maildir.Read(*local, e.Config.MaxMessageBytes)
	if err != nil {
		return err
	}
	if maildir.Hash(data) != op.Hash {
		return errors.New("local operation target was edited")
	}
	if err = e.runOperation(ctx, op, raw); err != nil {
		return err
	}
	if err = e.verify(ctx, op); err != nil {
		return err
	}
	current, err := e.Remote.Metadata(ctx, op.Result)
	if err != nil {
		return err
	}
	desired := MergeFlags(op.RemoteFlags, op.Flags, current.Flags)
	changedLocally := local.Flags ^ op.LocalFlags
	desired = (desired &^ changedLocally) | (local.Flags & changedLocally)
	if err = e.setFlags(ctx, op.Result, desired); err != nil {
		return err
	}
	updated, err := e.Local.Rename(*local, op.EntryID, desired)
	if err != nil {
		return err
	}
	entry := model.Entry{ReceivedAt: current.Date, ID: op.EntryID, Remote: op.Result, Agreed: desired, Hash: op.Hash, Size: updated.Size, ModTime: updated.ModTime}
	if err = e.State.Put("entry", entry.ID, entry); err != nil {
		return err
	}
	op.Applied = true
	if err = e.State.Put("operation", op.ID, op); err != nil {
		return err
	}
	return e.cleanOperation(op)
}

// Resolve records an operator-identified result. A later sync verifies the body
// against the journal hash before binding it to a local entry.
func Resolve(st *state.Store, id string, uid, validity uint32, notApplied bool) error {
	var op model.Operation
	found, err := st.Get("operation", id, &op)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("operation not found")
	}
	if op.Kind == "delete" {
		return errors.New("UID deletions recover automatically after a successful inventory; resolve the reported conflict and sync again")
	}
	if op.Phase != "unknown" && op.Phase != "inflight" && !(op.Phase == "done" && !op.Verified && !op.Applied) {
		return errors.New("only uncertain operations can be resolved")
	}
	if notApplied {
		op.Phase = "planned"
		op.Result = model.Ref{}
		op.Verified = false
	} else {
		if uid == 0 || validity == 0 {
			return errors.New("uid and uidvalidity are required")
		}
		op.Result = model.Ref{Folder: op.Destination, UID: uid, Validity: validity}
		op.Phase = "done"
		op.Verified = false
	}
	op.Error = ""
	return st.Put("operation", id, op)
}

// An already-known destination must not be silently recreated after removal.
func (e *Engine) ensureDestination(ctx context.Context, folder string) error {
	var prior model.Boundary
	found, err := e.State.Get("boundary", folder, &prior)
	if err != nil {
		return err
	}
	if !found {
		if err = e.Remote.Ensure(ctx, folder); err != nil {
			return err
		}
	}
	box, err := e.Remote.Snapshot(ctx, folder)
	if err != nil {
		return err
	}
	if found && box.Validity != prior.Validity {
		return errors.New("destination UIDVALIDITY changed")
	}
	if !found {
		return e.State.Put("boundary", folder, model.Boundary{Validity: box.Validity, NextUID: box.NextUID})
	}
	return nil
}

func (e *Engine) cleanArrival(a *model.Arrival) error {
	if err := e.Local.Unstage(a.ID); err != nil {
		return err
	}
	a.Cleaned = true
	return e.State.Put("arrival", a.Source.Ref.Key(), a)
}
func (e *Engine) cleanOperation(op *model.Operation) error {
	if err := e.Local.Unstage(op.IDSafe()); err != nil {
		return err
	}
	op.Cleaned = true
	return e.State.Put("operation", op.ID, op)
}

func (e *Engine) setFlags(ctx context.Context, ref model.Ref, want model.Flags) error {
	current, err := e.Remote.Metadata(ctx, ref)
	if err != nil {
		return err
	}
	add, remove := want&^current.Flags, current.Flags&^want
	if add|remove == 0 {
		return nil
	}
	if !e.Config.RemoteWrites {
		return errors.New("remote_writes is false")
	}
	return e.Remote.Flags(ctx, ref, add, remove)
}

// Persist one small observation per folder attempt. Message progress is derived
// from existing entry records rather than maintained as a second counter.
func (e *Engine) inventory(ctx context.Context, name string) (model.Mailbox, error) {
	p := status.FolderProgress{Name: name}
	if _, err := e.State.Get("folder_progress", name, &p); err != nil {
		return model.Mailbox{}, err
	}
	p.State = "inventory"
	p.Error = ""
	if err := e.State.Put("folder_progress", name, p); err != nil {
		return model.Mailbox{}, err
	}
	box, err := e.Remote.Snapshot(ctx, name)
	if err != nil {
		p.State = "error"
		p.Error = err.Error()
	} else {
		p.State = "observed"
		p.LastInventory = time.Now().UTC()
		p.RemoteMessages = len(box.Messages)
	}
	if save := e.State.Put("folder_progress", name, p); save != nil {
		return box, save
	}
	return box, err
}
