package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/sieve"
	"github.com/jp39/imaplocal/internal/state"
)

const fetchBatchBytes int64 = 16 << 20

func (e *Engine) downloadArrivals(ctx context.Context, remote map[string]model.Message,
	mapped, uncertainFolders map[string]bool, index map[string][]model.Local, arrivals []model.Arrival) error {
	observed, err := state.List[model.BodySize](e.State, "body_size")
	if err != nil {
		return err
	}
	bodySizes := map[string]int64{}
	for _, body := range observed {
		bodySizes[body.Ref.Key()] = body.Size
	}
	saved := map[string]bool{}
	for _, a := range arrivals {
		saved[a.Source.Ref.Key()] = true
	}
	refs := make([]string, 0, len(remote))
	for key := range remote {
		refs = append(refs, key)
	}
	sort.Slice(refs, func(i, j int) bool {
		a, b := remote[refs[i]].Ref, remote[refs[j]].Ref
		if a.Folder != b.Folder {
			return a.Folder < b.Folder
		}
		return a.UID < b.UID
	})
	processed := 0
	report := func(folder string) {
		processed++
		if e.Progress != nil {
			e.Progress("messages", folder, processed, len(refs))
		}
	}
	for i := 0; i < len(refs); {
		if err := ctx.Err(); err != nil {
			return err
		}
		var batch []model.Ref
		wanted := map[string]model.Message{}
		var bytes int64
		for i < len(refs) {
			key := refs[i]
			m := remote[key]
			if mapped[key] || saved[key] || uncertainFolders[m.Ref.Folder] {
				report(m.Ref.Folder)
				i++
				continue
			}
			size := max(m.Size, bodySizes[key])
			if size > e.Config.MaxMessageBytes {
				if err := e.issue(key, "message_too_large", fmt.Sprintf("%s UIDVALIDITY %d UID %d: body size %d exceeds limit %d", m.Ref.Folder, m.Ref.Validity, m.Ref.UID, size, e.Config.MaxMessageBytes)); err != nil {
					return err
				}
				report(m.Ref.Folder)
				i++
				continue
			}
			if len(batch) > 0 && (len(batch) == model.MaxFetchBatchMessages ||
				m.Ref.Folder != batch[0].Folder || m.Ref.Validity != batch[0].Validity || bytes+max(m.Size, 1) > fetchBatchBytes) {
				break
			}
			batch = append(batch, m.Ref)
			wanted[key] = m
			bytes += max(m.Size, 1)
			i++
		}
		if len(batch) == 0 {
			continue
		}
		var prepared []model.Arrival
		received := map[string]bool{}
		sink := func(r model.Ref, raw []byte, itemErr error) error {
			key := r.Key()
			m, ok := wanted[key]
			if !ok || received[key] {
				return errors.New("batch returned an unexpected or duplicate membership")
			}
			received[key] = true
			var tooLarge *model.BodyTooLargeError
			switch {
			case errors.As(itemErr, &tooLarge):
				if err := e.State.Put("body_size", key, model.BodySize{Ref: r, Size: tooLarge.Size}); err != nil {
					return err
				}
				bodySizes[key] = tooLarge.Size
				if err := e.issue(key, "message_too_large", fmt.Sprintf("%s UIDVALIDITY %d UID %d: %s", r.Folder, r.Validity, r.UID, tooLarge)); err != nil {
					return err
				}
			case errors.Is(itemErr, model.ErrRemoteMessageGone):
				// Complete inventory on the next cycle catches up with this expunge.
			case errors.Is(itemErr, model.ErrRemoteBodyUnavailable):
				if err := e.issue(key, "body_unavailable", fmt.Sprintf("%s UIDVALIDITY %d UID %d: body unavailable", r.Folder, r.Validity, r.UID)); err != nil {
					return err
				}
			case itemErr != nil:
				return itemErr
			default:
				a, err := e.prepareArrival(ctx, m, raw)
				if err != nil {
					return err
				}
				if a != nil {
					prepared = append(prepared, *a)
					saved[key] = true
					if bodySizes[key] != 0 {
						if err := e.State.Delete("body_size", key); err != nil {
							return err
						}
						delete(bodySizes, key)
					}
				}
			}
			report(r.Folder)
			return nil
		}
		var err error
		if backend, ok := e.Remote.(model.BatchFetcher); ok {
			err = backend.FetchBatch(ctx, batch, e.Config.MaxMessageBytes, sink)
		} else {
			// Simple adapters retain their single-message interface. A one-message
			// failure still keeps previous callbacks' durable staging and plans.
			for _, r := range batch {
				raw, itemErr := e.Remote.Fetch(ctx, r, e.Config.MaxMessageBytes)
				err = sink(r, raw, itemErr)
				if err != nil {
					err = fmt.Errorf("download %q UIDVALIDITY %d UID %d: %w", r.Folder, r.Validity, r.UID, err)
					break
				}
			}
		}
		if err != nil {
			return err
		}
		if len(received) != len(batch) {
			return errors.New("batch completed without reporting every requested membership")
		}
		// FETCH is fully drained. Filters may now issue MOVE/COPY/STORE on the same
		// connection without deadlocking its decoder or changing selection mid-batch.
		for i := range prepared {
			a := &prepared[i]
			if err = e.arrive(ctx, a, index); err != nil {
				if er := e.issue(a.ID, "arrival_blocked", err.Error()); er != nil {
					return er
				}
			}
		}
	}
	if e.Progress != nil {
		e.Progress("messages", "", len(refs), len(refs))
	}
	return nil
}

// prepareArrival has no remote effects. Stage and journal each complete body
// before accepting the next one, so a failed batch resumes from these plans.
func (e *Engine) prepareArrival(ctx context.Context, m model.Message, raw []byte) (*model.Arrival, error) {
	if raw == nil {
		return nil, errors.New("download returned no body")
	}
	if int64(len(raw)) > e.Config.MaxMessageBytes {
		return nil, errors.New("download exceeds max_message_bytes")
	}
	key := m.Ref.Key()
	e.messageEvent("message received", m.Ref, raw, "bytes", len(raw))
	var err error
	a := model.Arrival{ID: maildir.Hash([]byte(key))[:32], Source: m, Hash: maildir.Hash(raw)}
	targets := []sieve.Target{{Folder: m.Ref.Folder, Flags: m.Flags}}
	filter := false
	for _, f := range e.Config.FilterFolders {
		current, err := e.currentFolder(f)
		if err != nil {
			return nil, err
		}
		if current == m.Ref.Folder {
			filter = true
		}
	}
	if filter {
		e.messageEvent("filter running", m.Ref, raw, "rule_hash", e.Filter.Hash)
		targets, err = e.Filter.Evaluate(ctx, m.Ref.Folder, raw, m.Flags)
		if err != nil {
			e.messageEvent("filter failed", m.Ref, raw, "error", err)
			if er := e.issue(key, "filter_failed", err.Error()); er != nil {
				return nil, er
			}
			return nil, nil
		}
		a.RuleHash = e.Filter.Hash
		e.messageEvent("filter completed", m.Ref, raw, "targets", targets)
	}
	for _, target := range targets {
		current, err := e.currentFolder(target.Folder)
		if err != nil {
			return nil, err
		}
		if current == "" {
			return nil, errors.New("Sieve destination is a retired folder")
		}
		target.Folder = current
		a.Deliveries = append(a.Deliveries, model.Delivery{ID: model.NewID(), Folder: target.Folder, Flags: target.Flags, Date: m.Date})
	}
	if err = e.Local.Stage(a.ID, raw); err != nil {
		return nil, err
	}
	if err = e.State.Put("arrival", key, a); err != nil {
		return nil, err
	}

	return &a, nil
}
