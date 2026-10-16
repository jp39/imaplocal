package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
)

type unavailableBodies struct {
	*fake
	failures map[uint32]error
}

func (f *unavailableBodies) Fetch(ctx context.Context, r model.Ref, max int64) ([]byte, error) {
	if err := f.failures[r.UID]; err != nil {
		if errors.Is(err, model.ErrRemoteMessageGone) {
			delete(f.boxes[r.Folder], r.UID)
		}
		return nil, fmt.Errorf("fetch: %w", err)
	}
	return f.fake.Fetch(ctx, r, max)
}
func TestUnavailableArrivalsDoNotStarveOtherMessagesAndRetry(t *testing.T) {
	e, remote := setup(t, "")
	gone := remote.add("INBOX", fixture, 0)
	unavailable := remote.add("INBOX", fixture, 0)
	healthy := remote.add("INBOX", fixture, 0)
	backend := &unavailableBodies{fake: remote, failures: map[uint32]error{gone.UID: model.ErrRemoteMessageGone, unavailable.UID: model.ErrRemoteBodyUnavailable}}
	e.Remote = backend
	cycle(t, e)
	entries, err := state.List[model.Entry](e.State, "entry")
	if err != nil || len(entries) != 1 || entries[0].Remote != healthy {
		t.Fatalf("healthy mail blocked: %+v, %v", entries, err)
	}
	issues, err := state.List[model.Issue](e.State, "issue")
	if err != nil || len(issues) != 1 || issues[0].Kind != "body_unavailable" || !strings.Contains(issues[0].Detail, "INBOX UIDVALIDITY 1 UID 2") {
		t.Fatalf("unavailable body not visible: %+v, %v", issues, err)
	}
	// A later cycle retries the body, clears its issue and imports it exactly once.
	delete(backend.failures, unavailable.UID)
	cycle(t, e)
	if len(scan(t, e)) != 2 {
		t.Fatal("retry did not recover unavailable mail")
	}
	issues, err = state.List[model.Issue](e.State, "issue")
	if err != nil || len(issues) != 0 {
		t.Fatal("stale body issue", issues, err)
	}
	arrivals, err := state.List[model.Arrival](e.State, "arrival")
	if err != nil || len(arrivals) != 2 {
		t.Fatal("missing message was staged or recovery duplicated", err)
	}
}
func TestTransportFetchFailureStillStopsCycle(t *testing.T) {
	e, remote := setup(t, "")
	ref := remote.add("INBOX", fixture, 0)
	e.Remote = &unavailableBodies{fake: remote, failures: map[uint32]error{ref.UID: errors.New("connection lost")}}
	err := e.Cycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), `download "INBOX" UIDVALIDITY 1 UID 1`) {
		t.Fatal("transport failure masked", err)
	}
	if len(scan(t, e)) != 0 {
		t.Fatal("published incomplete download")
	}
}
