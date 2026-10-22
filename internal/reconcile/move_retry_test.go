package reconcile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
)

type interruptedMove struct {
	*fake
	failBefore bool
	attempts   int
	metadata   func(context.Context, model.Ref) (model.Message, error)
}

func (f *interruptedMove) Transfer(ctx context.Context, ref model.Ref, dest string, move bool) (model.Ref, error) {
	f.attempts++
	if f.failBefore {
		f.failBefore = false
		return model.Ref{}, io.ErrUnexpectedEOF
	}
	return f.fake.Transfer(ctx, ref, dest, move)
}

func (f *interruptedMove) Metadata(ctx context.Context, ref model.Ref) (model.Message, error) {
	if f.metadata != nil {
		return f.metadata(ctx, ref)
	}
	return f.fake.Metadata(ctx, ref)
}

func TestUncertainMoveRetriesSurvivingSourceAfterRestart(t *testing.T) {
	for _, mode := range []string{"sieve", "external"} {
		for _, phase := range []string{"unknown", "inflight"} {
			t.Run(mode+"/"+phase, func(t *testing.T) {
				script := ""
				if mode == "sieve" {
					script = `require "fileinto"; fileinto "Filed";`
				}
				e, f := setup(t, script)
				ref := f.add("INBOX", fixture, model.Seen|model.Flagged)
				if mode == "external" {
					cycle(t, e)
					local := scan(t, e)[0]
					if err := e.Local.Ensure("Filed"); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(local.Path, filepath.Join(e.Local.Root, "Filed", "cur", filepath.Base(local.Path))); err != nil {
						t.Fatal(err)
					}
				}
				backend := &interruptedMove{fake: f, failBefore: true}
				e.Remote = backend
				cycle(t, e)
				ops, err := state.List[model.Operation](e.State, "operation")
				if err != nil || len(ops) != 1 || ops[0].Phase != "unknown" || backend.attempts != 1 {
					t.Fatalf("failed attempt not retained: %+v, %v", ops, err)
				}
				if len(f.boxes["Filed"]) != 0 || len(f.boxes["INBOX"]) != 1 {
					t.Fatal("failed request changed remote mail")
				}
				// Both lost replies and process death while inflight use the same
				// recovery path. No manual resolve is needed for a surviving UID.
				op := ops[0]
				op.Phase = phase
				if err := e.State.Put("operation", op.ID, op); err != nil {
					t.Fatal(err)
				}
				c := e.Config
				e.Close()
				e, err = Open(c)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Close()
				e.Remote = backend
				var logs bytes.Buffer
				e.Log = slog.New(slog.NewTextHandler(&logs, nil))
				cycle(t, e)
				if backend.attempts != 2 || f.copies != 1 || len(f.boxes["INBOX"]) != 0 || len(f.boxes["Filed"]) != 1 {
					t.Fatal("MOVE did not recover exactly once")
				}
				locals := scan(t, e)
				if len(locals) != 1 || locals[0].Folder != "Filed" || locals[0].Flags != model.Seen|model.Flagged {
					t.Fatalf("recovered publication wrong: %+v", locals)
				}
				var got model.Operation
				if _, err := e.State.Get("operation", op.ID, &got); err != nil || got.Phase != "done" || !got.Verified || got.Error != "" || got.Source != ref || !got.Result.Valid() {
					t.Fatalf("recovered operation wrong: %+v, %v", got, err)
				}
				if !strings.Contains(logs.String(), "retrying uncertain MOVE; original source still exists") || !strings.Contains(logs.String(), "message_id=<same@example.org>") {
					t.Fatal("missing diagnostic retry event")
				}
				cycle(t, e)
				if backend.attempts != 2 || len(scan(t, e)) != 1 {
					t.Fatal("completed MOVE was repeated or duplicated")
				}
			})
		}
	}
}

func TestUncertainMoveChecksBeforeRetry(t *testing.T) {
	for _, scenario := range []string{"missing source", "changed epoch", "lookup failure", "wrong identity", "writes disabled", "canceled", "copy", "append"} {
		t.Run(scenario, func(t *testing.T) {
			e, f := setup(t, "")
			ref := f.add("INBOX", fixture, 0)
			backend := &interruptedMove{fake: f}
			e.Remote = backend
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			op := model.Operation{ID: "uncertain", Kind: "move", Source: ref, Destination: "Filed", Phase: "unknown", Hash: maildir.Hash(fixture), Error: "lost response"}
			switch scenario {
			case "missing source":
				delete(f.boxes["INBOX"], ref.UID)
				f.add("INBOX", fixture, 0) // Same bytes/Message-ID, different UID.
				f.add("Filed", fixture, 0) // A lookalike is not a result mapping.
			case "changed epoch":
				f.epochs["INBOX"]++
			case "lookup failure":
				backend.metadata = func(context.Context, model.Ref) (model.Message, error) {
					return model.Message{}, io.ErrUnexpectedEOF
				}
			case "wrong identity":
				backend.metadata = func(context.Context, model.Ref) (model.Message, error) {
					return model.Message{Ref: model.Ref{Folder: "INBOX", Validity: ref.Validity, UID: ref.UID + 1}}, nil
				}
			case "writes disabled":
				e.Config.RemoteWrites = false
			case "canceled":
				backend.metadata = func(ctx context.Context, r model.Ref) (model.Message, error) {
					cancel()
					return f.Metadata(ctx, r)
				}
			case "copy", "append":
				op.Kind = scenario
				backend.metadata = func(context.Context, model.Ref) (model.Message, error) {
					t.Fatal("COPY/APPEND must not use MOVE source recovery")
					return model.Message{}, nil
				}
			}
			if err := e.State.Put("operation", op.ID, op); err != nil {
				t.Fatal(err)
			}
			err := e.runOperation(ctx, &op, fixture)
			if err == nil || backend.attempts != 0 || f.copies != 0 || f.appends != 0 {
				t.Fatalf("unverified operation was retried: %v", err)
			}
			if scenario == "lookup failure" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal("lookup failure lost its cause", err)
			}
			var saved model.Operation
			if _, err := e.State.Get("operation", op.ID, &saved); err != nil || saved.Phase != "unknown" || saved.Error != "lost response" || saved.Result.Valid() {
				t.Fatalf("uncertainty was cleared: %+v, %v", saved, err)
			}
		})
	}
}

func TestUncertainMoveRetryCanStillLoseAcknowledgement(t *testing.T) {
	e, f := setup(t, "")
	ref := f.add("INBOX", fixture, 0)
	backend := &interruptedMove{fake: f, failBefore: true}
	e.Remote = backend
	op := model.Operation{ID: "lost-twice", Kind: "move", Source: ref, Destination: "Filed", Hash: maildir.Hash(fixture)}
	if err := e.runOperation(context.Background(), &op, fixture); err == nil {
		t.Fatal("first failed request was not surfaced")
	}
	f.lose = true // The retry executes, but its reply is lost.
	if err := e.runOperation(context.Background(), &op, fixture); err == nil {
		t.Fatal("lost retry acknowledgement was not surfaced")
	}
	if err := e.runOperation(context.Background(), &op, fixture); err == nil {
		t.Fatal("absent source was treated as a known destination")
	}
	if backend.attempts != 2 || f.copies != 1 || len(f.boxes["Filed"]) != 1 || len(f.boxes["INBOX"]) != 0 || op.Phase != "unknown" || op.Result.Valid() {
		t.Fatalf("lost retry was duplicated or guessed: %+v", op)
	}
}

func TestUncertainMoveSourceDisappearsAfterCheck(t *testing.T) {
	e, f := setup(t, "")
	ref := f.add("INBOX", fixture, 0)
	backend := &interruptedMove{fake: f}
	backend.metadata = func(ctx context.Context, r model.Ref) (model.Message, error) {
		message, err := f.Metadata(ctx, r)
		delete(f.boxes["INBOX"], ref.UID)
		return message, err
	}
	e.Remote = backend
	op := model.Operation{ID: "concurrent-removal", Kind: "move", Source: ref, Destination: "Filed", Phase: "unknown", Hash: maildir.Hash(fixture)}
	if err := e.State.Put("operation", op.ID, op); err != nil {
		t.Fatal(err)
	}
	if err := e.runOperation(context.Background(), &op, fixture); err == nil {
		t.Fatal("source disappearance produced a guessed success")
	}
	var saved model.Operation
	if _, err := e.State.Get("operation", op.ID, &saved); err != nil || saved.Phase != "unknown" || saved.Result.Valid() || f.copies != 0 {
		t.Fatalf("concurrent disappearance lost uncertainty: %+v, %v", saved, err)
	}
}
