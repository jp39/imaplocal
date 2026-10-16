package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
)

type batchingFake struct {
	*fake
	batches   [][]model.Ref
	fetched   map[string]int
	active    bool
	failAfter int
}

func (f *batchingFake) FetchBatch(ctx context.Context, refs []model.Ref, max int64, sink model.BodySink) error {
	f.batches = append(f.batches, append([]model.Ref(nil), refs...))
	f.active = true
	defer func() { f.active = false }()
	for i, r := range refs {
		if f.failAfter > 0 && i == f.failAfter {
			f.failAfter = 0
			return io.ErrUnexpectedEOF
		}
		if f.fetched == nil {
			f.fetched = map[string]int{}
		}
		f.fetched[r.Key()]++
		raw, err := f.fake.Fetch(ctx, r, max)
		if e := sink(r, raw, err); e != nil {
			return e
		}
	}
	return nil
}
func (f *batchingFake) Transfer(ctx context.Context, r model.Ref, dest string, move bool) (model.Ref, error) {
	if f.active {
		return model.Ref{}, errors.New("filter issued IMAP during streaming FETCH")
	}
	return f.fake.Transfer(ctx, r, dest, move)
}
func TestDownloadsAreBoundedAndGroupedByMailbox(t *testing.T) {
	e, f := setup(t, "")
	e.Config.Folders = []string{"INBOX", "Archive"}
	for i := 0; i < 130; i++ {
		f.add("INBOX", []byte(fmt.Sprintf("Subject: %d\r\n\r\nbody %d", i, i)), 0)
	}
	for i := 0; i < 3; i++ {
		f.add("Archive", fixture, 0)
	}
	remote := &batchingFake{fake: f}
	e.Remote = remote
	cycle(t, e)
	if len(remote.batches) != 4 || len(scan(t, e)) != 133 {
		t.Fatal("unexpected batching/import count", len(remote.batches))
	}
	for _, batch := range remote.batches {
		if len(batch) > model.MaxFetchBatchMessages {
			t.Fatal("unbounded batch")
		}
		for _, r := range batch {
			if r.Folder != batch[0].Folder || r.Validity != batch[0].Validity {
				t.Fatal("mixed mailbox batch")
			}
		}
	}
	remote.batches = nil
	cycle(t, e)
	if len(remote.batches) != 0 {
		t.Fatal("completed messages downloaded again")
	}
}
func TestInterruptedBatchResumesDurableBodiesAfterRestart(t *testing.T) {
	e, f := setup(t, "")
	for i := 0; i < 5; i++ {
		f.add("INBOX", fixture, 0)
	}
	remote := &batchingFake{fake: f, failAfter: 2}
	e.Remote = remote
	if err := e.Cycle(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("expected interrupted batch", err)
	}
	arrivals, err := state.List[model.Arrival](e.State, "arrival")
	if err != nil || len(arrivals) != 2 {
		t.Fatal("complete body checkpoints lost", len(arrivals), err)
	}
	for _, a := range arrivals {
		if _, err = e.Local.Staged(a.ID); err != nil {
			t.Fatal(err)
		}
	}
	cfg := e.Config
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.Remote = remote
	cycle(t, resumed)
	if len(scan(t, resumed)) != 5 {
		t.Fatal("batch restart lost or duplicated mail")
	}
	for key, n := range remote.fetched {
		if n != 1 {
			t.Fatal("checkpointed body downloaded again", key, n)
		}
	}
}
func TestBatchByteBudgetAndFiltersWaitForFetchCompletion(t *testing.T) {
	e, f := setup(t, `require ["fileinto"]; fileinto "INBOX/Filed";`)
	remote := &batchingFake{fake: f}
	e.Remote = remote
	cycle(t, e) // Establish an empty mailbox before arrivals.
	for i := 0; i < 3; i++ {
		r := f.add("INBOX", fixture, 0)
		msg := f.boxes[r.Folder][r.UID]
		msg.message.Size = 10 << 20
		f.boxes[r.Folder][r.UID] = msg
	}
	cycle(t, e)
	if len(remote.batches) != 3 {
		t.Fatal("batch byte budget ignored", len(remote.batches))
	}
	if f.copies != 3 {
		t.Fatal("filter did not run after FETCH", f.copies)
	}
	for _, l := range scan(t, e) {
		if l.Folder != "INBOX/Filed" {
			t.Fatal("filter ran with active fetch", l.Folder)
		}
	}
}

type oversizedBatch struct {
	*fake
	attempts map[uint32]int
	actual   int64
	badUID   uint32
}

func (f *oversizedBatch) FetchBatch(ctx context.Context, refs []model.Ref, max int64, sink model.BodySink) error {
	for _, r := range refs {
		f.attempts[r.UID]++
		if r.UID == f.badUID && f.actual > max {
			tooLarge := &model.BodyTooLargeError{Size: f.actual, Limit: max}
			if err := sink(r, nil, tooLarge); err != nil {
				return err
			}
			return tooLarge // Connection is aborted, as for a real unconsumed literal.
		}
		raw, err := f.fake.Fetch(ctx, r, max)
		if err = sink(r, raw, err); err != nil {
			return err
		}
	}
	return nil
}
func TestUnderstatedOversizedBodyDoesNotBlockLaterMailAcrossRestart(t *testing.T) {
	e, f := setup(t, "")
	e.Config.MaxMessageBytes = 256
	f.add("INBOX", fixture, 0)
	large := append(append([]byte(nil), fixture...), make([]byte, 2048-len(fixture))...)
	bad := f.add("INBOX", large, 0)
	msg := f.boxes["INBOX"][bad.UID]
	msg.message.Size = 100
	f.boxes["INBOX"][bad.UID] = msg
	f.add("INBOX", fixture, 0)
	remote := &oversizedBatch{fake: f, attempts: map[uint32]int{}, actual: int64(len(large)), badUID: bad.UID}
	e.Remote = remote
	var tooLarge *model.BodyTooLargeError
	if err := e.Cycle(context.Background()); !errors.As(err, &tooLarge) {
		t.Fatal("missing initial size checkpoint", err)
	}
	cfg := e.Config
	e.Close()
	resumed, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.Remote = remote
	cycle(t, resumed)
	if len(scan(t, resumed)) != 2 || remote.attempts[bad.UID] != 1 {
		t.Fatal("known oversized UID still blocks or gets downloaded again", remote.attempts)
	}
	issues, err := state.List[model.Issue](resumed.State, "issue")
	if err != nil || len(issues) != 1 || issues[0].Kind != "message_too_large" {
		t.Fatal("oversized message not visible", issues, err)
	}
	sizes, err := state.List[model.BodySize](resumed.State, "body_size")
	if err != nil || len(sizes) != 1 || sizes[0].Ref != bad || sizes[0].Size != int64(len(large)) {
		t.Fatal("size observation lost", sizes, err)
	}
	// A higher limit retries the original UID without changing its mailbox epoch.
	resumed.Config.MaxMessageBytes = 4096
	cycle(t, resumed)
	if len(scan(t, resumed)) != 3 || remote.attempts[bad.UID] != 2 {
		t.Fatal("raising limit did not recover skipped message", remote.attempts)
	}
	sizes, err = state.List[model.BodySize](resumed.State, "body_size")
	if err != nil || len(sizes) != 0 {
		t.Fatal("completed size checkpoint not cleaned", sizes, err)
	}
	issues, err = state.List[model.Issue](resumed.State, "issue")
	if err != nil || len(issues) != 0 {
		t.Fatal("completed limit issue not cleared", issues, err)
	}
}
