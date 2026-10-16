package imap

import (
	"context"
	"errors"
	"fmt"
	"io"

	protocol "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jp39/imaplocal/internal/model"
)

// FetchBatch selects once and streams complete bodies into local checkpoints.
// A sink must not run IMAP commands until the entire batch has returned.
func (c *Client) FetchBatch(ctx context.Context, refs []model.Ref, max int64, sink model.BodySink) error {
	if len(refs) == 0 {
		return nil
	}
	if len(refs) > model.MaxFetchBatchMessages || max < 1 || sink == nil {
		return errors.New("invalid body fetch batch size, limit or sink")
	}
	first := refs[0]
	requested := map[uint32]bool{}
	var set protocol.UIDSet
	for _, r := range refs {
		if !r.Valid() || r.Folder != first.Folder || r.Validity != first.Validity || requested[r.UID] {
			return errors.New("body fetch batch requires unique UIDs from one mailbox and UIDVALIDITY")
		}
		requested[r.UID] = true
		set.AddNum(protocol.UID(r.UID))
	}
	err := c.run(ctx, func() error {
		if err := c.selectRef(first); err != nil {
			return err
		}
		cmd := c.client.Fetch(set, &protocol.FetchOptions{UID: true, BodySection: []*protocol.FetchItemBodySection{{Peek: true}}})
		complete := false
		literalFailed := false
		// A failed sink/literal must close the socket BEFORE draining the command;
		// otherwise its decoder can block on an unread following literal.
		defer func() {
			if !complete {
				_ = c.conn.Close()
				if literalFailed {
					// Its EOF already released the decoder. Re-discarding that
					// truncated reader would race with decoder cleanup in beta.8.
					<-c.client.Closed()
					return
				}
			}
			_ = cmd.Close()
		}()
		delivered := map[uint32]bool{}
		for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
			var uid uint32
			var raw []byte
			for item := msg.Next(); item != nil; item = msg.Next() {
				switch item := item.(type) {
				case imapclient.FetchItemDataUID:
					uid = uint32(item.UID)
				case imapclient.FetchItemDataBodySection:
					if item.Literal == nil {
						continue
					}
					if raw != nil {
						return errors.New("FETCH returned multiple full bodies for one message")
					}
					size := item.Literal.Size()
					if size < 0 {
						return errors.New("negative IMAP literal size")
					}
					if size > max {
						if !requested[uid] || delivered[uid] {
							return fmt.Errorf("oversized FETCH has unexpected or duplicate UID %d", uid)
						}
						tooLarge := &model.BodyTooLargeError{Size: size, Limit: max}
						r := model.Ref{Folder: first.Folder, Validity: first.Validity, UID: uid}
						// Checkpoint the actual size before closing rather than drain
						// a huge literal only to discard it. The next connection skips
						// this UID unless the configured limit has been increased.
						if err := sink(r, nil, tooLarge); err != nil {
							return err
						}
						return fmt.Errorf("UID %d: %w", uid, tooLarge)
					}
					body := make([]byte, size)
					if _, err := io.ReadFull(item.Literal, body); err != nil {
						literalFailed = true
						return fmt.Errorf("UID %d body: %w", uid, err)
					}
					raw = body
				}
			}
			if raw == nil {
				continue
			} // Missing/NIL bodies are checked after draining FETCH.
			if !requested[uid] || delivered[uid] {
				return fmt.Errorf("FETCH body has unexpected, missing or duplicate UID %d", uid)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			r := model.Ref{Folder: first.Folder, Validity: first.Validity, UID: uid}
			if err := sink(r, raw, nil); err != nil {
				return err
			}
			delivered[uid] = true
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		complete = true
		// Confirm absent bodies together. Exchange may acknowledge a body request
		// without exporting an existing item; that is not proof of an expunge.
		var missing protocol.UIDSet
		for _, r := range refs {
			if !delivered[r.UID] {
				missing.AddNum(protocol.UID(r.UID))
			}
		}
		if len(missing) == 0 {
			return nil
		}
		metadata, err := c.client.Fetch(missing, &protocol.FetchOptions{UID: true}).Collect()
		if err != nil {
			return err
		}
		present := map[uint32]bool{}
		for _, m := range metadata {
			present[uint32(m.UID)] = true
		}
		for _, r := range refs {
			if delivered[r.UID] {
				continue
			}
			itemErr := model.ErrRemoteMessageGone
			if present[r.UID] {
				itemErr = model.ErrRemoteBodyUnavailable
			}
			if err := sink(r, nil, itemErr); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("fetch batch %q UIDVALIDITY %d: %w", first.Folder, first.Validity, err)
	}
	return nil
}
