package reconcile

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/sieve"
	"github.com/jp39/imaplocal/internal/state"
)

func TestInitialFilteringOnceAndDiagnosticIdentity(t *testing.T) {
	e, f := setup(t, `require ["fileinto", "copy"]; fileinto :copy "Filed"; keep;`)
	var logs bytes.Buffer
	e.Log = slog.New(slog.NewTextHandler(&logs, nil))
	r := f.add("INBOX", fixture, 0)
	cycle(t, e)
	if f.copies != 1 || len(scan(t, e)) != 2 {
		t.Fatal("initial inventory was not filtered")
	}
	for _, want := range []string{"message received", "filter running", "filter completed", "remote message operation completed", "message published to Maildir", "message_id=<same@example.org>", "filename=", "uidvalidity=1", "uid=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %q in logs: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "Unmodified body") {
		t.Fatal("body leaked into logs")
	}
	c := e.Config
	e.Close()
	if err := os.WriteFile(c.Sieve, []byte(`require "fileinto"; fileinto "Other";`), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.Remote = f
	f.Flags(context.Background(), r, model.Seen, 0)
	cycle(t, reopened)
	if f.copies != 1 || len(f.boxes["Other"]) != 0 {
		t.Fatal("restart, rule edit or flag change reran completed filtering")
	}
}

func TestManualFilterPreviewApplyAndRecovery(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "lost acknowledgement"}[lost], func(t *testing.T) {
			e, f := setup(t, `keep;`)
			r := f.add("INBOX", fixture, model.Seen)
			cycle(t, e)
			original := scan(t, e)[0]
			if err := os.WriteFile(e.Config.Sieve, []byte(`require "fileinto"; fileinto "Filed";`), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			e.Filter, err = sieve.Load(e.Config.Sieve)
			if err != nil {
				t.Fatal(err)
			}
			// Offline preview can coexist with a writer and must not retire the old entry.
			view, err := View(e.Config)
			if err != nil {
				t.Fatal(err)
			}
			preview, err := view.PlanFilters(context.Background(), FilterOptions{MessageIDs: []string{"<same@example.org>"}})
			view.Close()
			if err != nil || preview.Matched != 1 || len(preview.Plans) != 1 {
				t.Fatalf("preview: %+v %v", preview, err)
			}
			if f.copies != 0 {
				t.Fatal("preview mutated IMAP")
			}
			if _, err = os.Stat(original.Path); err != nil {
				t.Fatal("preview changed local mail", err)
			}
			f.lose = lost
			err = e.ApplyFilters(context.Background(), preview.Plans)
			if lost {
				if err == nil {
					t.Fatal("lost acknowledgement was not surfaced")
				}
				ops, _ := state.List[model.Operation](e.State, "operation")
				if len(ops) != 1 || ops[0].Phase != "unknown" {
					t.Fatalf("uncertain operation not journaled: %+v", ops)
				}
				c := e.Config
				e.Close()
				e, err = Open(c)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Close()
				e.Remote = f
				if err = Resolve(e.State, ops[0].ID, 1, 1, false); err != nil {
					t.Fatal(err)
				}
				cycle(t, e)
			} else if err != nil {
				t.Fatal(err)
			}
			locals := scan(t, e)
			if len(locals) != 1 || locals[0].Folder != "Filed" || f.copies != 1 || len(f.boxes[r.Folder]) != 0 {
				t.Fatalf("incorrect result: %+v copies=%d", locals, f.copies)
			}
			raw, err := maildir.Read(locals[0], e.Config.MaxMessageBytes)
			if err != nil || !bytes.Equal(raw, fixture) {
				t.Fatal("raw bytes changed", err)
			}
			arrivals, _ := state.List[model.Arrival](e.State, "arrival")
			for _, a := range arrivals {
				if !a.Done || !a.Cleaned {
					t.Fatalf("unfinished arrival: %+v", a)
				}
			}
			cycle(t, e)
			if f.copies != 1 {
				t.Fatal("completed manual filter repeated")
			}
		})
	}
}

func TestManualFilterCopyKeepAndFlags(t *testing.T) {
	e, f := setup(t, `keep;`)
	f.add("INBOX", fixture, 0)
	cycle(t, e)
	if err := os.WriteFile(e.Config.Sieve, []byte(`require ["fileinto", "copy", "imap4flags"]; addflag "\\Seen"; fileinto :copy "Filed"; keep;`), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	e.Filter, err = sieve.Load(e.Config.Sieve)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := e.PlanFilters(context.Background(), FilterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.ApplyFilters(context.Background(), preview.Plans); err != nil {
		t.Fatal(err)
	}
	locals := scan(t, e)
	if len(locals) != 2 || f.copies != 1 {
		t.Fatalf("incorrect copies: %+v", locals)
	}
	for _, l := range locals {
		if l.Flags != model.Seen {
			t.Fatalf("flags not applied: %+v", l)
		}
	}
	cycle(t, e)
	if f.copies != 1 {
		t.Fatal("copy repeated after recovery")
	}
}

func TestManualMessageSelectionIgnoresUnrelatedMalformedMail(t *testing.T) {
	e, f := setup(t, `keep;`)
	f.add("INBOX", fixture, 0)
	f.add("INBOX", []byte("${Subject}:\r\n\r\nmalformed unrelated mail\r\n"), 0)
	// Import without filtering so the malformed fixture represents an existing
	// completed message from a prior configuration.
	e.Config.FilterFolders = nil
	cycle(t, e)
	preview, err := e.PlanFilters(context.Background(), FilterOptions{Folders: []string{"INBOX"}, MessageIDs: []string{"same@example.org"}})
	if err != nil || preview.Matched != 1 {
		t.Fatalf("unrelated malformed mail blocked selection: %+v %v", preview, err)
	}
}

func TestManualFilterProcessesMalformedHeaderPreservesBytes(t *testing.T) {
	e, f := setup(t, `keep;`)
	f.add("INBOX", fixture, model.Seen)
	malformed := []byte("${Subject}:\r\nTo: reviews@example.org\r\nMessage-ID: <malformed@example.org>\r\n\r\nkeep these bytes\r\n")
	bad := f.add("INBOX", malformed, model.Seen)
	e.Config.FilterFolders = nil
	cycle(t, e)
	if err := os.WriteFile(e.Config.Sieve, []byte(`require "fileinto"; if header :contains "to" "reviews@example.org" { fileinto "Filed"; }`), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	e.Filter, err = sieve.Load(e.Config.Sieve)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := e.PlanFilters(context.Background(), FilterOptions{Folders: []string{"INBOX"}, MessageIDs: []string{"malformed@example.org"}})
	if err != nil || len(selected.Plans) != 1 || selected.Plans[0].Entry.Remote != bad {
		t.Fatalf("Message-ID after malformed field lost: %+v %v", selected, err)
	}
	preview, err := e.PlanFilters(context.Background(), FilterOptions{Folders: []string{"INBOX"}})
	if err != nil || len(preview.Plans) != 2 || len(preview.Skipped) != 0 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if err = e.ApplyFilters(context.Background(), preview.Plans); err != nil {
		t.Fatal(err)
	}
	if len(f.boxes["INBOX"]) != 0 || f.copies != 2 {
		t.Fatal("messages not filed")
	}
	found := false
	for _, local := range scan(t, e) {
		raw, err := maildir.Read(local, e.Config.MaxMessageBytes)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(raw, malformed) {
			found = true
		}
		if local.Folder != "Filed" || (!bytes.Equal(raw, malformed) && !bytes.Equal(raw, fixture)) {
			t.Fatal("message bytes changed")
		}
	}
	if !found {
		t.Fatal("original malformed message bytes not preserved")
	}
}

func TestArrivalFiltersValidHeadersAfterMalformedLine(t *testing.T) {
	e, f := setup(t, `require "fileinto"; if header :contains "to" "reviews@example.org" { fileinto "Filed"; }`)
	var logs bytes.Buffer
	e.Log = slog.New(slog.NewTextHandler(&logs, nil))
	raw := []byte("${Subject}:\r\nTo: reviews@example.org\r\nMessage-ID: <malformed-arrival@example.org>\r\n\r\nbody\r\n")
	f.add("INBOX", raw, model.Seen)
	cycle(t, e)
	locals := scan(t, e)
	if len(locals) != 1 || locals[0].Folder != "Filed" {
		t.Fatalf("arrival not filtered: %+v", locals)
	}
	stored, err := maildir.Read(locals[0], e.Config.MaxMessageBytes)
	if err != nil || !bytes.Equal(raw, stored) {
		t.Fatal("raw arrival changed", err)
	}
	if !strings.Contains(logs.String(), "ignored_headers=1") || !strings.Contains(logs.String(), "message_id=<malformed-arrival@example.org>") {
		t.Fatal("missing parse diagnostics", logs.String())
	}
}

func TestManualFilterReceivedDateSelectionAndLegacyJournal(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current entries", true: "legacy entries"}[legacy], func(t *testing.T) {
			e, f := setup(t, `keep;`)
			cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			for i, date := range []time.Time{cutoff.Add(-time.Second), cutoff, cutoff.Add(time.Second)} {
				// Deliberately contradict the received date: the sender date must have
				// no effect on selection.
				raw := append([]byte("Date: Wed, 1 Jan 2020 00:00:00 +0000\r\n"), fixture...)
				if i == 0 {
					raw = append([]byte("Date: Tue, 1 Jan 2030 00:00:00 +0000\r\n"), fixture...)
				}
				r := f.add("INBOX", raw, model.Seen)
				item := f.boxes["INBOX"][r.UID]
				item.message.Date = date
				f.boxes["INBOX"][r.UID] = item
			}
			cycle(t, e)
			if legacy {
				entries, _ := state.List[model.Entry](e.State, "entry")
				for _, entry := range entries {
					entry.ReceivedAt = time.Time{}
					if err := e.State.Put("entry", entry.ID, entry); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Touching files must not make old mail eligible or recent mail too old.
			for _, local := range scan(t, e) {
				if err := os.Chtimes(local.Path, cutoff.Add(-365*24*time.Hour), cutoff.Add(-365*24*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(e.Config.Sieve, []byte(`require "fileinto"; fileinto "Filed";`), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			e.Filter, err = sieve.Load(e.Config.Sieve)
			if err != nil {
				t.Fatal(err)
			}
			preview, err := e.PlanFilters(context.Background(), FilterOptions{Since: cutoff})
			if err != nil || preview.Matched != 2 || len(preview.Plans) != 2 || len(preview.Skipped) != 0 {
				t.Fatalf("date selection: %+v %v", preview, err)
			}
			for _, plan := range preview.Plans {
				if plan.Entry.Remote.UID == 1 {
					t.Fatal("old message selected")
				}
				if plan.ReceivedAt.Before(cutoff) || plan.ReceivedAt.IsZero() {
					t.Fatal("received date missing from plan")
				}
			}
			if err = e.ApplyFilters(context.Background(), preview.Plans); err != nil {
				t.Fatal(err)
			}
			if len(f.boxes["INBOX"]) != 1 || f.copies != 2 {
				t.Fatal("date-limited apply affected old mail")
			}
			if _, ok := f.boxes["INBOX"][1]; !ok {
				t.Fatal("old message moved")
			}
		})
	}
}

func TestManualFilterDoesNotGuessMissingReceivedDate(t *testing.T) {
	e, f := setup(t, `keep;`)
	f.add("INBOX", fixture, model.Seen)
	cycle(t, e)
	entries, _ := state.List[model.Entry](e.State, "entry")
	entry := entries[0]
	entry.ReceivedAt = time.Time{}
	if err := e.State.Put("entry", entry.ID, entry); err != nil {
		t.Fatal(err)
	}
	keys, _ := e.State.Keys("arrival")
	for _, key := range keys {
		if err := e.State.Delete("arrival", key); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := e.PlanFilters(context.Background(), FilterOptions{Since: time.Now().Add(-24 * time.Hour)})
	if err != nil || len(preview.Skipped) != 1 || preview.Matched != 0 {
		t.Fatalf("unknown timestamp guessed: %+v %v", preview, err)
	}
}
