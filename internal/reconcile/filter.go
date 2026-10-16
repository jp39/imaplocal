package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/message"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/sieve"
	"github.com/jp39/imaplocal/internal/state"
)

type FilterOptions struct {
	Folders, MessageIDs []string
	// Since includes messages received at or after this instant. Zero means all.
	Since time.Time
}
type FilterPlan struct {
	ReceivedAt         time.Time
	Entry              model.Entry
	Local              model.Local
	Targets            []sieve.Target
	MessageID, Subject string
}
type FilterSkip struct {
	ReceivedAt                  time.Time
	Ref                         model.Ref
	Filename, MessageID, Reason string
}
type FilterPreview struct {
	Plans   []FilterPlan
	Matched int
	Skipped []FilterSkip
}

func normalizedMessageID(id string) string { return strings.Trim(strings.TrimSpace(id), "<>") }

// PlanFilters is offline and read-only. It never changes processing records.
func (e *Engine) PlanFilters(ctx context.Context, opts FilterOptions) (FilterPreview, error) {
	var result FilterPreview
	if e.Filter.Hash == "" {
		return result, errors.New("filter requires a configured Sieve script")
	}
	folders := opts.Folders
	if len(folders) == 0 {
		folders = e.Config.FilterFolders
	}
	if len(folders) == 0 {
		return result, errors.New("specify -folder or configure filter_folders")
	}
	selected := map[string]bool{}
	for _, name := range folders {
		if config.FolderPattern(name) {
			return result, errors.New("filter -folder requires literal names")
		}
		if err := config.Folder(name); err != nil {
			return result, err
		}
		current, err := e.currentFolder(name)
		if err != nil {
			return result, err
		}
		if current == "" {
			return result, fmt.Errorf("filter folder %q is retired", name)
		}
		covered, err := e.configuredFolder(current)
		if err != nil {
			return result, err
		}
		if !covered {
			return result, fmt.Errorf("filter folder %q is outside configured folders", name)
		}
		selected[current] = true
	}
	wanted := map[string]bool{}
	for _, id := range opts.MessageIDs {
		id = normalizedMessageID(id)
		if id == "" || strings.ContainsAny(id, "\r\n") {
			return result, errors.New("invalid -message-id")
		}
		wanted[id] = false
	}
	entries, err := state.List[model.Entry](e.State, "entry")
	if err != nil {
		return result, err
	}
	locals, err := e.Local.Scan()
	if err != nil {
		return result, err
	}
	byID := group(locals)
	received := map[string]time.Time{}
	// Older entries lack ReceivedAt. Recover exact server timestamps from
	// immutable arrival plans without writing state or trusting file mtimes.
	missing := map[string]bool{}
	for _, entry := range entries {
		if !entry.Tombstone && selected[entry.Remote.Folder] && entry.ReceivedAt.IsZero() {
			missing[entry.ID] = true
		}
	}
	if len(missing) > 0 {
		arrivals, err := state.List[model.Arrival](e.State, "arrival")
		if err != nil {
			return result, err
		}
		for _, a := range arrivals {
			for _, d := range a.Deliveries {
				if missing[d.ID] {
					received[d.ID] = d.Date
				}
			}
		}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if entry.Tombstone || !selected[entry.Remote.Folder] {
			continue
		}
		files := byID[entry.ID]
		date := entry.ReceivedAt
		if date.IsZero() {
			date = received[entry.ID]
		}
		if !opts.Since.IsZero() {
			if date.IsZero() {
				path := ""
				if len(files) == 1 {
					path = files[0].Path
				}
				result.Skipped = append(result.Skipped, FilterSkip{Ref: entry.Remote, Filename: path, Reason: "received timestamp unavailable; synchronize with the updated daemon first"})
				continue
			}
			if date.Before(opts.Since) {
				continue
			}
		}
		if len(files) != 1 || files[0].Folder != entry.Remote.Folder {
			return result, fmt.Errorf("%s: local move/copy/removal is pending; synchronize first", entry.ID)
		}
		local := files[0]
		raw, err := maildir.Read(local, e.Config.MaxMessageBytes)
		if err != nil {
			return result, err
		}
		header, _ := message.Headers(raw)
		id := normalizedMessageID(header.Get("Message-ID"))
		if len(wanted) > 0 {
			if _, ok := wanted[id]; !ok {
				continue
			}
			wanted[id] = true
		}
		if local.Deleted || local.Flags != entry.Agreed || maildir.Hash(raw) != entry.Hash {
			return result, fmt.Errorf("%s: local flags/body/deletion differ from sync state; synchronize first", entry.ID)
		}
		result.Matched++
		e.messageEvent("filter running", entry.Remote, raw, "filename", local.Path, "manual", true)
		targets, err := e.Filter.Evaluate(ctx, entry.Remote.Folder, raw, local.Flags)
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			result.Skipped = append(result.Skipped, FilterSkip{ReceivedAt: date, Ref: entry.Remote, Filename: local.Path, MessageID: id, Reason: err.Error()})
			continue
		}
		e.messageEvent("filter completed", entry.Remote, raw, "filename", local.Path, "manual", true, "targets", targets)
		for i := range targets {
			current, err := e.currentFolder(targets[i].Folder)
			if err != nil {
				return result, err
			}
			if current == "" {
				return result, fmt.Errorf("Sieve destination %q is retired", targets[i].Folder)
			}
			targets[i].Folder = current
		}
		if len(targets) == 1 && targets[0].Folder == entry.Remote.Folder && targets[0].Flags == local.Flags {
			continue
		}
		result.Plans = append(result.Plans, FilterPlan{ReceivedAt: date, Entry: entry, Local: local, Targets: targets, MessageID: id, Subject: header.Get("Subject")})
	}
	for id, found := range wanted {
		if !found {
			return result, fmt.Errorf("Message-ID <%s> not found in selected folders", id)
		}
	}
	sort.Slice(result.Plans, func(i, j int) bool {
		a, b := result.Plans[i].Entry.Remote, result.Plans[j].Entry.Remote
		if a.Folder != b.Folder {
			return a.Folder < b.Folder
		}
		return a.UID < b.UID
	})
	return result, nil
}

// CheckFilterReady requires completed recovery before scheduling a new opt-in
// run. Once journaled, a manual plan resumes through the normal arrival path.
func (e *Engine) CheckFilterReady() error {
	arrivals, err := state.List[model.Arrival](e.State, "arrival")
	if err != nil {
		return err
	}
	for _, a := range arrivals {
		if !a.Done || !a.Cleaned {
			return errors.New("unfinished arrivals exist; run sync before applying filters")
		}
	}
	ops, err := state.List[model.Operation](e.State, "operation")
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Phase != "done" || (op.ArrivalID == "" && !op.Applied) {
			return errors.New("unfinished message operations exist; run sync/resolve first")
		}
	}
	folders, err := state.List[model.FolderOperation](e.State, "folder_operation")
	if err != nil {
		return err
	}
	for _, op := range folders {
		if op.Phase != "done" {
			return errors.New("unfinished folder operations exist; run sync/resolve first")
		}
	}
	return nil
}

func (e *Engine) ApplyFilters(ctx context.Context, plans []FilterPlan) error {
	if len(plans) == 0 {
		return nil
	}
	if !e.Config.RemoteWrites {
		return errors.New("filter -apply requires remote_writes=true")
	}
	if e.Remote == nil {
		return errors.New("filter -apply requires an IMAP connection")
	}
	if err := e.CheckFilterReady(); err != nil {
		return err
	}
	locals, err := e.Local.Scan()
	if err != nil {
		return err
	}
	index := group(locals)
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return err
		}
		var current model.Entry
		found, err := e.State.Get("entry", plan.Entry.ID, &current)
		if err != nil {
			return err
		}
		if !found || current != plan.Entry {
			return errors.New("filter source mapping changed; preview again")
		}
		meta, err := e.Remote.Metadata(ctx, current.Remote)
		if err != nil {
			return err
		}
		if meta.Ref != current.Remote || meta.Flags != current.Agreed {
			return errors.New("filter source changed remotely; synchronize first")
		}
		raw, err := maildir.Read(plan.Local, e.Config.MaxMessageBytes)
		if err != nil {
			return err
		}
		if maildir.Hash(raw) != current.Hash {
			return errors.New("filter source body changed")
		}
		a := model.Arrival{ID: model.NewID(), OriginID: current.ID, Source: meta, Hash: current.Hash, RuleHash: e.Filter.Hash}
		for _, target := range plan.Targets {
			a.Deliveries = append(a.Deliveries, model.Delivery{ID: model.NewID(), Folder: target.Folder, Flags: target.Flags, Date: meta.Date})
		}
		if err = e.checkFilterOrigin(&a, index); err != nil {
			return err
		}
		if err = e.Local.Stage(a.ID, raw); err != nil {
			return err
		}
		if err = e.State.Put("arrival", a.Source.Ref.Key(), a); err != nil {
			return err
		}
		if err = e.arrive(ctx, &a, index); err != nil {
			if save := e.issue(a.ID, "arrival_blocked", err.Error()); save != nil {
				return save
			}
			return err
		}
	}
	return nil
}

func (e *Engine) checkFilterOrigin(a *model.Arrival, index map[string][]model.Local) error {
	var origin model.Entry
	found, err := e.State.Get("entry", a.OriginID, &origin)
	if err != nil {
		return err
	}
	if !found || origin.Remote != a.Source.Ref || origin.Hash != a.Hash {
		return errors.New("manual filter source identity changed")
	}
	if origin.Tombstone {
		return nil
	} // Cleanup may have committed before a restart.
	files := index[a.OriginID]
	if len(files) != 1 || files[0].Folder != a.Source.Ref.Folder || files[0].Deleted || files[0].Flags != a.Source.Flags {
		return errors.New("manual filter source moved/copied/deleted or flags changed; local content retained")
	}
	raw, err := maildir.Read(files[0], e.Config.MaxMessageBytes)
	if err != nil {
		return err
	}
	if maildir.Hash(raw) != a.Hash {
		return errors.New("manual filter source body changed; local content retained")
	}
	return nil
}
