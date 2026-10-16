package reconcile

import (
	"context"
	"errors"
	"fmt"

	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/model"
)

// Journal intent, then confirm it against a fresh complete local scan before
// issuing a request in this same cycle. A rename across directories may cross
// the initial scan boundary; an artificial extra-cycle delay is unnecessary.
func (e *Engine) planDelete(ctx context.Context, entry model.Entry, remote model.Message, locals []model.Local) error {
	if !e.Config.RemoteWrites {
		return errors.New("local deletion retained; remote_writes is false")
	}
	if _, ok := e.Remote.(model.MessageDeleter); !ok {
		return errors.New("backend does not support targeted message deletion")
	}
	if remote.Flags != entry.Agreed {
		return errors.New("local deletion conflicts with remote flag changes")
	}
	if err := e.Local.CheckFolder(entry.Remote.Folder); err != nil {
		return err
	}
	if len(locals) != 0 {
		if err := e.deletionIntent(entry, locals); err != nil {
			return err
		}
	}
	op := model.Operation{ID: "delete:" + model.NewID(), Kind: "delete", EntryID: entry.ID, Source: entry.Remote, Flags: entry.Agreed, Hash: entry.Hash, Phase: "planned"}
	if err := e.State.Put("operation", op.ID, op); err != nil {
		return err
	}
	return e.resumeDelete(ctx, &op)
}

func (e *Engine) deletionIntent(entry model.Entry, locals []model.Local) error {
	if err := e.Local.CheckFolder(entry.Remote.Folder); err != nil {
		return err
	}
	if len(locals) == 0 {
		return nil
	}
	if len(locals) != 1 || locals[0].Folder != entry.Remote.Folder || !locals[0].Deleted {
		return errors.New("message reappeared, was copied, or was moved; deletion paused")
	}
	l := locals[0]
	if l.Flags != entry.Agreed {
		return errors.New("deletion conflicts with local flag changes")
	}
	raw, err := maildir.Read(l, e.Config.MaxMessageBytes)
	if err != nil {
		return err
	}
	if maildir.Hash(raw) != entry.Hash {
		return errors.New("deletion conflicts with a local body edit")
	}
	return nil
}

func (e *Engine) resumeDelete(ctx context.Context, op *model.Operation) error {
	var entry model.Entry
	found, err := e.State.Get("entry", op.EntryID, &entry)
	if err != nil {
		return err
	}
	if !found || entry.Remote != op.Source {
		return errors.New("deletion mapping changed")
	}
	box, err := e.Remote.Snapshot(ctx, op.Source.Folder)
	if err != nil {
		return err
	}
	if box.Validity != op.Source.Validity {
		return errors.New("UIDVALIDITY changed; deletion paused")
	}
	present := false
	for _, m := range box.Messages {
		if m.Ref == op.Source {
			present = true
			if m.Flags != op.Flags {
				return errors.New("deletion conflicts with remote flag changes")
			}
		}
	}
	index, err := e.Local.Scan()
	if err != nil {
		return err
	}
	locals := group(index)[entry.ID]
	if present {
		intentErr := e.deletionIntent(entry, locals)
		if intentErr != nil {
			if op.Phase != "planned" {
				return intentErr
			}
			// No request was sent. A reappearing file cancels the old intent, allowing
			// normal flag or move reconciliation to run in this same cycle.
			op.Phase, op.Applied, op.Cleaned = "done", true, true
			op.Error = "deletion cancelled: local intent changed"
			return e.State.Put("operation", op.ID, op)
		}
		if !e.Config.RemoteWrites {
			return errors.New("remote_writes is false")
		}
		deleter, ok := e.Remote.(model.MessageDeleter)
		if !ok {
			return errors.New("backend does not support targeted message deletion")
		}
		if checker, ok := e.Remote.(interface {
			CheckOperation(context.Context, model.Operation) error
		}); ok {
			if err = checker.CheckOperation(ctx, *op); err != nil {
				return err
			}
		}
		op.Phase = "inflight"
		if err = e.State.Put("operation", op.ID, op); err != nil {
			return err
		}
		if e.Log != nil {
			e.Log.Info("remote message deletion started", "folder", op.Source.Folder, "uidvalidity", op.Source.Validity, "uid", op.Source.UID, "entry_id", entry.ID, "operation_id", op.ID)
		}
		if err = deleter.DeleteMessage(ctx, op.Source, op.Flags); err != nil {
			op.Phase, op.Error = "unknown", "targeted deletion failed; next inventory will verify its outcome"
			if save := e.State.Put("operation", op.ID, op); save != nil {
				return save
			}
			return fmt.Errorf("targeted deletion: %w", err)
		}
	}
	// An absent UID proves completion even after a crash/lost response. A
	// present UID can be retried safely: STORE +Deleted and UID EXPUNGE target
	// only this UID in the verified epoch, never any unrelated deleted mail.
	op.Phase, op.Verified, op.Error = "done", true, ""
	if err = e.State.Put("operation", op.ID, op); err != nil {
		return err
	}
	// An undeleted/restored file after a lost request is new local intent.
	// Keep that conflict durable rather than deleting it on the next cycle.
	for _, l := range locals {
		if !l.Deleted {
			entry.DeletionConflict = true
		}
	}
	if err = e.remoteRemoval(entry, locals); err != nil {
		return err
	}
	op.Applied, op.Cleaned = true, true
	return e.State.Put("operation", op.ID, op)
}

// Remote MOVE is observed as disappearance plus a new membership. Remove only
// an unchanged home copy; never match by Message-ID or body hash across UIDs.
func (e *Engine) remoteRemoval(entry model.Entry, locals []model.Local) error {
	entry.Tombstone = true
	if err := e.State.Put("entry", entry.ID, entry); err != nil {
		return err
	}
	if len(locals) == 0 {
		return nil
	}
	if entry.DeletionConflict && !locals[0].Deleted {
		return e.issue(entry.ID, "delete_conflict", "local message restored after remote deletion; retain it, or explicitly mark it T/remove it to accept deletion")
	}
	if len(locals) != 1 || locals[0].Folder != entry.Remote.Folder || locals[0].Flags != entry.Agreed {
		return e.issue(entry.ID, "delete_conflict", "remote membership disappeared during a local flag change, move, or copy; local content retained")
	}
	if err := e.Local.Remove(locals[0], entry.Hash, e.Config.MaxMessageBytes); err != nil {
		return e.issue(entry.ID, "delete_conflict", "remote membership disappeared; local content retained: "+err.Error())
	}
	if e.Log != nil {
		e.Log.Info("message removed from Maildir", "folder", entry.Remote.Folder, "uidvalidity", entry.Remote.Validity, "uid", entry.Remote.UID, "filename", locals[0].Path)
	}
	return nil
}
