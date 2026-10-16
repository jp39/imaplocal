package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
)

type stored struct {
	message model.Message
	raw     []byte
}
type fake struct {
	boxes           map[string]map[uint32]stored
	next            map[string]uint32
	epochs          map[string]uint32
	copies, appends int
	lose            bool
}

func newFake() *fake {
	return &fake{boxes: map[string]map[uint32]stored{}, next: map[string]uint32{}, epochs: map[string]uint32{}}
}
func (f *fake) Ensure(_ context.Context, folder string) error {
	if f.boxes[folder] == nil {
		f.boxes[folder] = map[uint32]stored{}
		f.next[folder] = 1
		f.epochs[folder] = 1
	}
	return nil
}
func (f *fake) Snapshot(ctx context.Context, folder string) (model.Mailbox, error) {
	f.Ensure(ctx, folder)
	out := model.Mailbox{Folder: folder, Validity: f.epochs[folder], NextUID: f.next[folder]}
	for _, s := range f.boxes[folder] {
		out.Messages = append(out.Messages, s.message)
	}
	return out, nil
}
func (f *fake) add(folder string, raw []byte, flags model.Flags) model.Ref {
	f.Ensure(context.Background(), folder)
	id := f.next[folder]
	f.next[folder]++
	r := model.Ref{Folder: folder, Validity: f.epochs[folder], UID: id}
	f.boxes[folder][id] = stored{model.Message{Ref: r, Flags: flags, Size: int64(len(raw)), Date: time.Now()}, append([]byte(nil), raw...)}
	return r
}
func (f *fake) Fetch(_ context.Context, r model.Ref, _ int64) ([]byte, error) {
	s, ok := f.boxes[r.Folder][r.UID]
	if !ok || r.Validity != f.epochs[r.Folder] {
		return nil, errors.New("missing message")
	}
	return append([]byte(nil), s.raw...), nil
}
func (f *fake) Flags(_ context.Context, r model.Ref, add, remove model.Flags) error {
	s := f.boxes[r.Folder][r.UID]
	s.message.Flags = (s.message.Flags &^ remove) | add
	f.boxes[r.Folder][r.UID] = s
	return nil
}
func (f *fake) Append(_ context.Context, folder string, raw []byte, flags model.Flags, _ time.Time) (model.Ref, error) {
	f.appends++
	r := f.add(folder, raw, flags)
	if f.lose {
		f.lose = false
		return model.Ref{}, errors.New("connection lost after APPEND")
	}
	return r, nil
}
func (f *fake) Transfer(_ context.Context, r model.Ref, dest string, move bool) (model.Ref, error) {
	s, ok := f.boxes[r.Folder][r.UID]
	if !ok {
		return model.Ref{}, errors.New("missing source")
	}
	f.copies++
	result := f.add(dest, s.raw, s.message.Flags)
	if move {
		delete(f.boxes[r.Folder], r.UID)
	}
	if f.lose {
		f.lose = false
		return model.Ref{}, errors.New("connection lost after mutation")
	}
	return result, nil
}
func (f *fake) Close() error { return nil }

var fixture = []byte("From: sender@example.org\r\nTo: reviews@example.org\r\nSubject: Review\r\nMessage-ID: <same@example.org>\r\n\r\nUnmodified body\r\n")

func setup(t *testing.T, script string) (*Engine, *fake) {
	t.Helper()
	root := t.TempDir()
	c := config.Config{Server: "example.org:993", Username: "test", CredentialCommand: []string{"unused"}, Maildir: filepath.Join(root, "mail"), Folders: []string{"INBOX"}, FilterFolders: []string{"INBOX"}, RemoteWrites: true}
	if script != "" {
		c.Sieve = filepath.Join(root, "rules.sieve")
		if err := os.WriteFile(c.Sieve, []byte(script), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	e.Remote = f
	t.Cleanup(func() { e.Close() })
	return e, f
}
func cycle(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func scan(t *testing.T, e *Engine) []model.Local {
	t.Helper()
	v, err := e.Local.Scan()
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestBootstrapFlagsAndExternalMoves(t *testing.T) {
	e, f := setup(t, "")
	r := f.add("INBOX", fixture, model.Flagged)
	cycle(t, e)
	locals := scan(t, e)
	if len(locals) != 1 || locals[0].Folder != "INBOX" {
		t.Fatalf("initial import unexpected: %+v", locals)
	}
	l := locals[0]
	// Acknowledge, mark read locally, and concurrently mark answered remotely.
	dest := filepath.Join(filepath.Dir(filepath.Dir(l.Path)), "cur", filepath.Base(l.Path))
	if err := os.Rename(l.Path, dest); err != nil {
		t.Fatal(err)
	}
	l.Path = dest
	l, err := e.Local.Rename(l, l.ID, l.Flags|model.Seen)
	if err != nil {
		t.Fatal(err)
	}
	f.Flags(context.Background(), r, model.Answered, 0)
	cycle(t, e)
	locals = scan(t, e)
	want := model.Seen | model.Answered | model.Flagged
	if locals[0].Flags != want || f.boxes["INBOX"][r.UID].message.Flags != want {
		t.Fatal("independent flag changes were lost")
	}
	// Toggling read back off must clear it on both sides.
	_, err = e.Local.Rename(locals[0], l.ID, want&^model.Seen)
	if err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if f.boxes["INBOX"][r.UID].message.Flags&model.Seen != 0 {
		t.Fatal("read flag did not clear")
	}
	locals = scan(t, e)
	if err = e.Local.Ensure("Archive"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(e.Local.Root, "Archive", "cur", filepath.Base(locals[0].Path))
	if err = os.Rename(locals[0].Path, target); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if len(f.boxes["INBOX"]) != 0 || len(f.boxes["Archive"]) != 1 || len(scan(t, e)) != 1 {
		t.Fatal("external move did not converge")
	}
	cycle(t, e)
	if f.copies != 1 {
		t.Fatal("move was repeated")
	}
}
func TestArrivalCopiesAndRestartPublication(t *testing.T) {
	e, f := setup(t, `require ["fileinto", "copy", "imap4flags"]; addflag "\\Seen"; fileinto :copy "INBOX/Review/Mine"; fileinto "INBOX/Review";`)
	cycle(t, e)
	r := f.add("INBOX", fixture, model.Flagged)
	cycle(t, e)
	if len(f.boxes["INBOX"]) != 0 || len(scan(t, e)) != 2 || f.copies != 2 {
		t.Fatalf("copy/move did not converge: %d copies, %+v", f.copies, scan(t, e))
	}
	var a model.Arrival
	if ok, err := e.State.Get("arrival", r.Key(), &a); err != nil || !ok {
		t.Fatal(err)
	}
	for _, l := range scan(t, e) {
		raw, err := maildir.Read(l, 1<<20)
		if err != nil || string(raw) != string(fixture) {
			t.Fatal("body altered")
		}
		if l.Flags != model.Flagged|model.Seen {
			t.Fatal("flags lost")
		}
	}
	// Simulate publication completed but the last arrival commit was lost.
	a.Done = false
	a.Cleaned = false
	if err := e.Local.Stage(a.ID, fixture); err != nil {
		t.Fatal(err)
	}
	if err := e.State.Put("arrival", r.Key(), a); err != nil {
		t.Fatal(err)
	}
	c := e.Config
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.Remote = f
	cycle(t, reopened)
	if len(scan(t, reopened)) != 2 || f.copies != 2 {
		t.Fatal("recovery duplicated delivery or remote copies")
	}
}
func TestUncertainAppendIsNeverRetried(t *testing.T) {
	e, f := setup(t, "")
	cycle(t, e)
	if err := e.Local.Ensure("INBOX"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Local.Root, "INBOX", "cur", "external:2,F")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	f.lose = true
	cycle(t, e)
	cycle(t, e)
	if f.appends != 1 || len(f.boxes["INBOX"]) != 1 {
		t.Fatal("uncertain APPEND was repeated")
	}
	ops, err := state.List[model.Operation](e.State, "operation")
	if err != nil || len(ops) != 1 || ops[0].Phase != "unknown" {
		t.Fatalf("unknown outcome not retained: %+v, %v", ops, err)
	}
	// A manually identified UID is verified against content before binding.
	if err = Resolve(e.State, ops[0].ID, 1, 1, false); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	cycle(t, e)
	if len(scan(t, e)) != 1 || f.appends != 1 {
		t.Fatal("resolution duplicated local or remote mail")
	}
}
func TestLostMoveResponsePausesAndResolves(t *testing.T) {
	e, f := setup(t, "")
	f.add("INBOX", fixture, 0)
	cycle(t, e)
	l := scan(t, e)[0]
	e.Local.Ensure("Archive")
	if err := os.Rename(l.Path, filepath.Join(e.Local.Root, "Archive", "cur", filepath.Base(l.Path))); err != nil {
		t.Fatal(err)
	}
	f.lose = true
	cycle(t, e)
	cycle(t, e)
	if f.copies != 1 {
		t.Fatal("uncertain MOVE repeated")
	}
	ops, _ := state.List[model.Operation](e.State, "operation")
	if len(ops) != 1 || ops[0].Phase != "unknown" {
		t.Fatalf("lost MOVE not journaled: %+v", ops)
	}
	if err := Resolve(e.State, ops[0].ID, 1, 1, false); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if len(scan(t, e)) != 1 {
		t.Fatal("lost MOVE resolution duplicated mail")
	}
}
func TestUIDValidityChangeStopsBeforeWrites(t *testing.T) {
	e, f := setup(t, "")
	f.add("INBOX", fixture, 0)
	cycle(t, e)
	f.epochs["INBOX"] = 2
	if err := e.Cycle(context.Background()); err == nil {
		t.Fatal("UIDVALIDITY reset was ignored")
	}
	if f.copies != 0 || f.appends != 0 || len(scan(t, e)) != 1 {
		t.Fatal("reset mutated mail")
	}
}
func TestMissingRemoteDoesNotResurrectLocal(t *testing.T) {
	e, f := setup(t, "")
	r := f.add("INBOX", fixture, 0)
	cycle(t, e)
	delete(f.boxes["INBOX"], r.UID)
	cycle(t, e)
	cycle(t, e)
	if f.appends != 0 || len(scan(t, e)) != 0 {
		t.Fatal("deleted remote resurrected or unchanged local copy retained")
	}
	entries, _ := state.List[model.Entry](e.State, "entry")
	if len(entries) != 1 || !entries[0].Tombstone {
		t.Fatal("missing tombstone")
	}
}
func TestIdenticalMessagesRemainDistinct(t *testing.T) {
	e, f := setup(t, "")
	f.add("INBOX", fixture, 0)
	f.add("INBOX", fixture, 0)
	cycle(t, e)
	if len(scan(t, e)) != 2 {
		t.Fatal("legitimate duplicate messages collapsed")
	}
}
func TestOwnershipRejectsExistingMaildirAndWrongAccount(t *testing.T) {
	root := t.TempDir()
	mail := filepath.Join(root, "mail")
	os.MkdirAll(filepath.Join(mail, "INBOX", "cur"), 0700)
	_, err := maildir.New(mail, filepath.Join(root, "private"), "owner")
	if err == nil {
		t.Fatal("existing directory was adopted")
	}
	e, _ := setup(t, "")
	c := e.Config
	e.Close()
	c.Username = "other"
	if reopened, err := Open(c); err == nil {
		reopened.Close()
		t.Fatal("account identity silently changed")
	}
}

func TestExternalCopyGetsDistinctMembershipAndCanBeRepeated(t *testing.T) {
	e, f := setup(t, "")
	f.add("INBOX", fixture, 0)
	cycle(t, e)
	original := scan(t, e)[0]
	if err := e.Local.Ensure("Archive"); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(e.Local.Root, "Archive", "cur", filepath.Base(original.Path))
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(copyPath, fixture, 0600); err != nil {
			t.Fatal(err)
		}
		cycle(t, e)
		cycle(t, e)
	}
	if f.copies != 2 || len(f.boxes["Archive"]) != 2 || len(scan(t, e)) != 3 {
		t.Fatalf("copies collapsed or repeated: %d, %+v", f.copies, scan(t, e))
	}
}
func TestInFlightBecomesUnknownAfterRestart(t *testing.T) {
	e, f := setup(t, "")
	cycle(t, e)
	op := model.Operation{ID: "interrupted", Kind: "append", Destination: "INBOX", EntryID: model.NewID(), Phase: "inflight", Hash: maildir.Hash(fixture)}
	if err := e.State.Put("operation", op.ID, op); err != nil {
		t.Fatal(err)
	}
	c := e.Config
	e.Close()
	reopened, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.Remote = f
	var restored model.Operation
	if _, err = reopened.State.Get("operation", op.ID, &restored); err != nil || restored.Phase != "unknown" {
		t.Fatal("inflight request became retryable", err)
	}
}
func TestInvalidResolutionCannotBindWrongMessage(t *testing.T) {
	e, f := setup(t, "")
	cycle(t, e)
	e.Local.Ensure("INBOX")
	path := filepath.Join(e.Local.Root, "INBOX", "cur", "external:2,")
	os.WriteFile(path, fixture, 0600)
	f.lose = true
	cycle(t, e)
	wrong := f.add("INBOX", []byte("From: wrong@example.org\r\n\r\nwrong"), 0)
	ops, _ := state.List[model.Operation](e.State, "operation")
	if err := Resolve(e.State, ops[0].ID, wrong.UID, wrong.Validity, false); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	entries, _ := state.List[model.Entry](e.State, "entry")
	for _, entry := range entries {
		if entry.Remote == wrong && entry.Hash == maildir.Hash(fixture) {
			t.Fatal("wrong body was bound to local mail")
		}
	}
	// An unverified operator resolution can be corrected without editing SQLite.
	if err := Resolve(e.State, ops[0].ID, 1, 1, false); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if f.appends != 1 {
		t.Fatal("resolution retried APPEND")
	}
}
func TestCompletedStagingIsReclaimed(t *testing.T) {
	e, f := setup(t, "")
	f.add("INBOX", fixture, 0)
	cycle(t, e)
	files, err := os.ReadDir(filepath.Join(e.Config.StateDir(), "staging"))
	if err != nil || len(files) != 0 {
		t.Fatal("completed download retained a second body copy", err)
	}
}

func (f *fake) Metadata(_ context.Context, r model.Ref) (model.Message, error) {
	s, ok := f.boxes[r.Folder][r.UID]
	if !ok || f.epochs[r.Folder] != r.Validity {
		return model.Message{}, errors.New("missing membership")
	}
	return s.message, nil
}

func TestCrashAfterPublicationBeforeMappingDoesNotUploadOrDuplicate(t *testing.T) {
	e, f := setup(t, "")
	cycle(t, e)
	ref := f.add("INBOX", fixture, model.Flagged)
	message := f.boxes["INBOX"][ref.UID].message
	a := model.Arrival{ID: maildir.Hash([]byte(ref.Key()))[:32], Source: message, Hash: maildir.Hash(fixture), Deliveries: []model.Delivery{{ID: model.NewID(), Folder: "INBOX", Flags: model.Flagged, Remote: ref}}}
	if err := e.Local.Stage(a.ID, fixture); err != nil {
		t.Fatal(err)
	}
	if err := e.State.Put("arrival", ref.Key(), a); err != nil {
		t.Fatal(err)
	}
	l, err := e.Local.Publish(a.Deliveries[0], fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	acknowledged := filepath.Join(e.Local.Root, "INBOX", "cur", filepath.Base(l.Path))
	if err = os.Rename(l.Path, acknowledged); err != nil {
		t.Fatal(err)
	}
	c := e.Config
	e.Close()
	reopened, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.Remote = f
	cycle(t, reopened)
	locals := scan(t, reopened)
	if len(locals) != 1 || locals[0].Path != acknowledged || f.appends != 0 {
		t.Fatal("orphan publication was duplicated or uploaded")
	}
	entries, err := state.List[model.Entry](reopened.State, "entry")
	if err != nil || len(entries) != 1 || entries[0].Remote != ref {
		t.Fatal("publication mapping not recovered", err)
	}
}
func TestCrashAfterAcknowledgedCopyBeforeDeliveryCheckpoint(t *testing.T) {
	e, f := setup(t, "")
	cycle(t, e)
	ref := f.add("INBOX", fixture, 0)
	message := f.boxes["INBOX"][ref.UID].message
	a := model.Arrival{ID: model.NewID(), Source: message, Hash: maildir.Hash(fixture), Deliveries: []model.Delivery{{ID: model.NewID(), Folder: "INBOX/Copy"}, {ID: model.NewID(), Folder: "INBOX/Final"}}}
	if err := e.Local.Stage(a.ID, fixture); err != nil {
		t.Fatal(err)
	}
	if err := e.State.Put("arrival", ref.Key(), a); err != nil {
		t.Fatal(err)
	}
	result, err := f.Transfer(context.Background(), ref, "INBOX/Copy", false)
	if err != nil {
		t.Fatal(err)
	}
	op := model.Operation{ID: a.ID + ":0", ArrivalID: a.ID, Kind: "copy", Source: ref, Destination: "INBOX/Copy", EntryID: a.Deliveries[0].ID, Hash: a.Hash, Phase: "done", Result: result, Verified: true}
	if err = e.State.Put("operation", op.ID, op); err != nil {
		t.Fatal(err)
	}
	c := e.Config
	e.Close()
	reopened, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.Remote = f
	cycle(t, reopened)
	cycle(t, reopened)
	if f.copies != 2 || len(scan(t, reopened)) != 2 || len(f.boxes["INBOX"]) != 0 {
		t.Fatalf("acknowledged copy was replayed: %d copies", f.copies)
	}
}
func TestKnownRemoteFolderIsNotRecreated(t *testing.T) {
	e, f := setup(t, "")
	f.add("INBOX", fixture, 0)
	cycle(t, e)
	// A missing remote folder must be an inventory error, not permission to CREATE.
	wrapped := missingFolder{Backend: f}
	e.Remote = wrapped
	if err := e.Cycle(context.Background()); err == nil {
		t.Fatal("missing known folder was ignored")
	}
	if len(scan(t, e)) != 1 {
		t.Fatal("remote folder failure changed local mail")
	}
}

type missingFolder struct{ model.Backend }

func (m missingFolder) Ensure(context.Context, string) error {
	panic("must not recreate a known mailbox")
}
func (m missingFolder) Snapshot(context.Context, string) (model.Mailbox, error) {
	return model.Mailbox{}, errors.New("mailbox no longer exists")
}

func TestLocalFlagsChangedDuringUncertainUploadArePreserved(t *testing.T) {
	e, f := setup(t, "")
	cycle(t, e)
	e.Local.Ensure("INBOX")
	original := filepath.Join(e.Local.Root, "INBOX", "cur", "external:2,")
	if err := os.WriteFile(original, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	f.lose = true
	cycle(t, e)
	if err := os.Rename(original, original+"S"); err != nil {
		t.Fatal(err)
	}
	ops, _ := state.List[model.Operation](e.State, "operation")
	if err := Resolve(e.State, ops[0].ID, 1, 1, false); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	locals := scan(t, e)
	if len(locals) != 1 || locals[0].Flags&model.Seen == 0 || f.boxes["INBOX"][1].message.Flags&model.Seen == 0 {
		t.Fatal("new local intent was overwritten during operation recovery")
	}
}
