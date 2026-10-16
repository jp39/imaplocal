package imap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jp39/imaplocal/internal/auth"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/testutil"
)

// A scripted peer exercises response order and broken literals that a normal
// in-memory server does not produce. No real credentials/server are involved.
func batchPeer(t *testing.T, handle func(net.Conn, string, string)) *Client {
	t.Helper()
	conn, peer := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer peer.Close()
		fmt.Fprint(peer, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
		reader := bufio.NewReader(peer)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			tag, command, _ := strings.Cut(strings.TrimSpace(line), " ")
			handle(peer, tag, command)
		}
	}()
	c := &Client{conn: conn, client: imapclient.New(conn, nil), timeout: 2 * time.Second}
	t.Cleanup(func() { c.Close(); <-done })
	return c
}
func batchRefs(n int) []model.Ref {
	refs := make([]model.Ref, n)
	for i := range refs {
		refs[i] = model.Ref{Folder: "INBOX", Validity: 42, UID: uint32(i + 1)}
	}
	return refs
}
func batchSelected(peer net.Conn, tag string) {
	fmt.Fprintf(peer, "* 4 EXISTS\r\n* OK [UIDVALIDITY 42] epoch\r\n* OK [UIDNEXT 5] next\r\n%s OK selected\r\n", tag)
}

func TestBatchFetchUsesOneSelectionAndPreservesUIDBodyAssociation(t *testing.T) {
	selections, bodyCommands, metadataCommands := 0, 0, 0
	c := batchPeer(t, func(peer net.Conn, tag, command string) {
		switch {
		case strings.HasPrefix(command, "EXAMINE "):
			selections++
			batchSelected(peer, tag)
		case strings.Contains(command, "BODY.PEEK[]"):
			bodyCommands++
			// Responses may arrive out of requested UID order.
			fmt.Fprint(peer, "* 3 FETCH (UID 3 BODY[] {5}\r\nthree)\r\n")
			fmt.Fprint(peer, "* 1 FETCH (UID 1 BODY[] {3}\r\none)\r\n")
			fmt.Fprint(peer, "* 2 FETCH (UID 2 BODY[] NIL)\r\n")
			fmt.Fprintf(peer, "%s OK fetched\r\n", tag)
		case strings.HasPrefix(command, "UID FETCH "):
			metadataCommands++
			fmt.Fprintf(peer, "* 2 FETCH (UID 2)\r\n%s OK metadata\r\n", tag)
		default:
			fmt.Fprintf(peer, "%s BAD unexpected\r\n", tag)
		}
	})
	bodies := map[uint32]string{}
	failures := map[uint32]error{}
	err := c.FetchBatch(context.Background(), batchRefs(4), 1024, func(r model.Ref, raw []byte, err error) error {
		if err != nil {
			failures[r.UID] = err
		} else {
			bodies[r.UID] = string(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if bodies[1] != "one" || bodies[3] != "three" || len(bodies) != 2 {
		t.Fatal("body/UID mismatch", bodies)
	}
	if !errors.Is(failures[2], model.ErrRemoteBodyUnavailable) || !errors.Is(failures[4], model.ErrRemoteMessageGone) {
		t.Fatal("missing-body classification", failures)
	}
	// Synchronize peer completion before inspecting its counters.
	c.Close()
	if selections != 1 || bodyCommands != 1 || metadataCommands != 1 {
		t.Fatal("not batched", selections, bodyCommands, metadataCommands)
	}
}
func TestTruncatedBatchKeepsOnlyCompleteBodies(t *testing.T) {
	c := batchPeer(t, func(peer net.Conn, tag, command string) {
		if strings.HasPrefix(command, "EXAMINE ") {
			batchSelected(peer, tag)
			return
		}
		fmt.Fprint(peer, "* 1 FETCH (UID 1 BODY[] {3}\r\none)\r\n* 2 FETCH (UID 2 BODY[] {20}\r\npart")
		peer.Close()
	})
	got := map[uint32]string{}
	err := c.FetchBatch(context.Background(), batchRefs(2), 1024, func(r model.Ref, raw []byte, err error) error { got[r.UID] = string(raw); return err })
	if err == nil || len(got) != 1 || got[1] != "one" {
		t.Fatal("partial body accepted or checkpoint lost", got, err)
	}
}
func TestBatchSinkFailureClosesBeforeDrainingNextLiteral(t *testing.T) {
	c := batchPeer(t, func(peer net.Conn, tag, command string) {
		if strings.HasPrefix(command, "EXAMINE ") {
			batchSelected(peer, tag)
			return
		}
		fmt.Fprint(peer, "* 1 FETCH (UID 1 BODY[] {3}\r\none)\r\n* 2 FETCH (UID 2 BODY[] {1000000}\r\n")
		io.CopyN(peer, strings.NewReader(strings.Repeat("x", 1<<20)), 1000000)
	})
	failed := errors.New("disk full")
	done := make(chan error, 1)
	go func() {
		done <- c.FetchBatch(context.Background(), batchRefs(2), 1024, func(model.Ref, []byte, error) error { return failed })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, failed) {
			t.Fatal("sink failure lost", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deadlock draining aborted batch")
	}
}
func TestBatchValidatesMailboxEpochAndLimitBeforeSending(t *testing.T) {
	for _, refs := range [][]model.Ref{
		{{Folder: "INBOX", Validity: 42, UID: 1}, {Folder: "Other", Validity: 42, UID: 2}},
		{{Folder: "INBOX", Validity: 42, UID: 1}, {Folder: "INBOX", Validity: 43, UID: 2}},
		{{Folder: "INBOX", Validity: 42, UID: 1}, {Folder: "INBOX", Validity: 42, UID: 1}},
		batchRefs(model.MaxFetchBatchMessages + 1),
	} {
		c := &Client{}
		if err := c.FetchBatch(context.Background(), refs, 1024, func(model.Ref, []byte, error) error { return nil }); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
}

func TestTLSBatchPreservesRawBytesAndUnreadFlags(t *testing.T) {
	cfg, user, _ := testutil.IMAP(t)
	expected := map[uint32][]byte{}
	for i := 1; i <= model.MaxFetchBatchMessages; i++ {
		raw := []byte(fmt.Sprintf("From: test@example.invalid\r\nSubject: batch %d\r\n\r\nRaw UTF-8: café %d\r\n", i, i))
		result, err := user.Append("INBOX", literal{bytes.NewReader(raw)}, &protocol.AppendOptions{Flags: []protocol.Flag{protocol.FlagFlagged}})
		if err != nil {
			t.Fatal(err)
		}
		expected[uint32(result.UID)] = raw
	}
	cfg.RemoteWrites = false
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, cfg, &auth.Command{Args: cfg.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	box, err := client.Snapshot(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	var refs []model.Ref
	for _, m := range box.Messages {
		refs = append(refs, m.Ref)
	}
	count := 0
	err = client.FetchBatch(ctx, refs, 1<<20, func(r model.Ref, raw []byte, itemErr error) error {
		if itemErr != nil {
			return itemErr
		}
		if !bytes.Equal(raw, expected[r.UID]) {
			return errors.New("raw bytes or UID association changed")
		}
		count++
		return nil
	})
	if err != nil || count != len(expected) {
		t.Fatal("TLS batch failed", count, err)
	}
	box, err = client.Snapshot(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range box.Messages {
		if m.Flags != model.Flagged {
			t.Fatal("BODY.PEEK changed unread flags", m.Flags)
		}
	}
}

func TestOversizedLiteralReportsActualSizeBeforeClosing(t *testing.T) {
	c := batchPeer(t, func(peer net.Conn, tag, command string) {
		if strings.HasPrefix(command, "EXAMINE ") {
			batchSelected(peer, tag)
			return
		}
		// Do not send the huge body: handling the limit must not depend on draining it.
		fmt.Fprint(peer, "* 1 FETCH (UID 1 BODY[] {79451030}\r\n")
	})
	var reported *model.BodyTooLargeError
	done := make(chan error, 1)
	go func() {
		done <- c.FetchBatch(context.Background(), batchRefs(1), 64<<20, func(r model.Ref, raw []byte, err error) error {
			if r.UID != 1 || raw != nil || !errors.As(err, &reported) {
				return errors.New("size checkpoint missing or associated with wrong UID")
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		var tooLarge *model.BodyTooLargeError
		if !errors.As(err, &tooLarge) || reported == nil || reported.Size != 79451030 || reported.Limit != 64<<20 {
			t.Fatal("actual size lost", reported, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked draining oversized literal")
	}
}
