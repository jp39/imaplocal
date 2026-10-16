package imap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	protocol "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jp39/imaplocal/internal/auth"
	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/model"
)

type Client struct {
	conn    net.Conn
	client  *imapclient.Client
	timeout time.Duration
	writes  bool
}
type xoauth2 struct{ user, token string }

func (x xoauth2) Start() (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + x.user + "\x01auth=Bearer " + x.token + "\x01\x01"), nil
}
func (x xoauth2) Next([]byte) ([]byte, error) {
	return nil, errors.New("XOAUTH2 authentication rejected")
}

func Dial(ctx context.Context, c config.Config, credentials *auth.Command, notify chan<- struct{}) (*Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		b, e := os.ReadFile(c.CAFile)
		if e != nil {
			return nil, e
		}
		pool, e := x509.SystemCertPool()
		if e != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("ca_file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	host, _, err := net.SplitHostPort(c.Server)
	if err != nil {
		return nil, err
	}
	tlsConfig.ServerName = host
	cred, err := credentials.Credential(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := dialTLS(ctx, c, tlsConfig)
	if err != nil {
		return nil, err
	}
	signal := func() {
		if notify != nil {
			select {
			case notify <- struct{}{}:
			default:
			}
		}
	}
	opts := &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
		Mailbox: func(*imapclient.UnilateralDataMailbox) { signal() },
		Expunge: func(uint32) { signal() },
		Fetch:   func(m *imapclient.FetchMessageData) { m.Collect(); signal() },
	}}
	out := &Client{conn: conn, client: imapclient.New(conn, opts), timeout: c.CommandTimeout(), writes: c.RemoteWrites}
	err = out.run(ctx, func() error {
		if e := out.client.WaitGreeting(); e != nil {
			return e
		}
		if c.Auth == "plain" {
			return out.client.Login(c.Username, cred).Wait()
		}
		return out.client.Authenticate(xoauth2{c.Username, cred})
	})
	if err != nil {
		out.Close()
		return nil, errors.New("IMAP authentication or greeting failed")
	}
	return out, nil
}
func (c *Client) run(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Close() })
	defer stop()
	timer := time.AfterFunc(c.timeout, func() { _ = c.conn.Close() })
	defer timer.Stop()
	return fn()
}
func (c *Client) Close() error { return c.client.Close() }
func (c *Client) selectRef(r model.Ref) error {
	data, err := c.client.Select(r.Folder, &protocol.SelectOptions{ReadOnly: !c.writes}).Wait()
	if err != nil {
		return err
	}
	if data.UIDValidity != r.Validity {
		return errors.New("UIDVALIDITY changed; refusing stale operation")
	}
	return nil
}
func uid(uid uint32) protocol.UIDSet { return protocol.UIDSetNum(protocol.UID(uid)) }
func (c *Client) Snapshot(ctx context.Context, folder string) (box model.Mailbox, err error) {
	err = c.run(ctx, func() error {
		data, e := c.client.Select(folder, &protocol.SelectOptions{ReadOnly: !c.writes}).Wait()
		if e != nil {
			return e
		}
		if data.UIDValidity == 0 || data.UIDNext == 0 {
			return errors.New("server did not supply stable UIDVALIDITY/UIDNEXT")
		}
		box = model.Mailbox{Folder: folder, Validity: data.UIDValidity, NextUID: uint32(data.UIDNext)}
		if data.NumMessages == 0 {
			return nil
		}
		messages, e := c.client.Fetch(protocol.UIDSet{{Start: 1, Stop: 0}}, &protocol.FetchOptions{UID: true, Flags: true, RFC822Size: true, InternalDate: true}).Collect()
		if e != nil {
			return e
		}
		for _, m := range messages {
			if m.UID == 0 {
				return errors.New("FETCH response omitted UID")
			}
			box.Messages = append(box.Messages, model.Message{Ref: model.Ref{Folder: folder, Validity: box.Validity, UID: uint32(m.UID)}, Flags: decode(m.Flags), Size: m.RFC822Size, Date: m.InternalDate})
		}
		return nil
	})
	return
}
func (c *Client) Fetch(ctx context.Context, r model.Ref, max int64) (raw []byte, err error) {
	var itemErr error
	err = c.FetchBatch(ctx, []model.Ref{r}, max, func(_ model.Ref, body []byte, missing error) error {
		raw, itemErr = body, missing
		return nil
	})
	if err == nil {
		err = itemErr
	}
	if err != nil {
		err = fmt.Errorf("fetch %q UIDVALIDITY %d UID %d: %w", r.Folder, r.Validity, r.UID, err)
	}
	return
}
func decode(fs []protocol.Flag) model.Flags {
	var out model.Flags
	for _, f := range fs {
		switch strings.ToLower(string(f)) {
		case `\seen`:
			out |= model.Seen
		case `\answered`:
			out |= model.Answered
		case `\flagged`:
			out |= model.Flagged
		case `\draft`:
			out |= model.Draft
		case "$forwarded":
			out |= model.Forwarded
		}
	}
	return out
}
func encode(f model.Flags) []protocol.Flag {
	var out []protocol.Flag
	for _, p := range []struct {
		f model.Flags
		s protocol.Flag
	}{{model.Seen, protocol.FlagSeen}, {model.Answered, protocol.FlagAnswered}, {model.Flagged, protocol.FlagFlagged}, {model.Draft, protocol.FlagDraft}, {model.Forwarded, protocol.Flag("$Forwarded")}} {
		if f&p.f != 0 {
			out = append(out, p.s)
		}
	}
	return out
}
func (c *Client) Flags(ctx context.Context, r model.Ref, add, remove model.Flags) error {
	if !c.writes {
		return errors.New("remote writes disabled")
	}
	return c.run(ctx, func() error {
		data, e := c.client.Select(r.Folder, nil).Wait()
		if e != nil {
			return e
		}
		if data.UIDValidity != r.Validity {
			return errors.New("UIDVALIDITY changed")
		}
		for _, flag := range encode(add | remove) {
			supported := false
			for _, p := range data.PermanentFlags {
				if strings.EqualFold(string(p), string(flag)) || p == protocol.FlagWildcard {
					supported = true
				}
			}
			if !supported {
				return fmt.Errorf("server cannot persist flag %s", flag)
			}
		}
		for _, change := range []struct {
			flags model.Flags
			op    protocol.StoreFlagsOp
		}{{remove, protocol.StoreFlagsDel}, {add, protocol.StoreFlagsAdd}} {
			if change.flags != 0 {
				if e = c.client.Store(uid(r.UID), &protocol.StoreFlags{Op: change.op, Silent: true, Flags: encode(change.flags)}, nil).Close(); e != nil {
					return e
				}
			}
		}
		return nil
	})
}
func (c *Client) Ensure(ctx context.Context, folder string) error {
	return c.run(ctx, func() error {
		boxes, e := c.client.List("", folder, nil).Collect()
		if e != nil {
			return e
		}
		for _, b := range boxes {
			if b.Mailbox == folder {
				return nil
			}
		}
		if !c.writes {
			return errors.New("destination mailbox missing and remote writes disabled")
		}
		return c.client.Create(folder, nil).Wait()
	})
}
func (c *Client) Append(ctx context.Context, folder string, raw []byte, flags model.Flags, date time.Time) (result model.Ref, err error) {
	if !c.writes {
		return result, errors.New("remote writes disabled")
	}
	err = c.run(ctx, func() error {
		if !c.client.Caps().Has(protocol.CapUIDPlus) {
			return errors.New("APPEND requires UIDPLUS; no UID guessing")
		}
		cmd := c.client.Append(folder, int64(len(raw)), &protocol.AppendOptions{Flags: encode(flags), Time: date})
		_, we := cmd.Write(raw)
		ce := cmd.Close()
		if we != nil {
			return we
		}
		if ce != nil {
			return ce
		}
		data, e := cmd.Wait()
		if e != nil {
			return e
		}
		result = model.Ref{Folder: folder, Validity: data.UIDValidity, UID: uint32(data.UID)}
		if !result.Valid() {
			return errors.New("APPEND succeeded without UID mapping")
		}
		return nil
	})
	return
}
func (c *Client) Transfer(ctx context.Context, r model.Ref, dest string, move bool) (result model.Ref, err error) {
	if !c.writes {
		return result, errors.New("remote writes disabled")
	}
	err = c.run(ctx, func() error {
		if !c.client.Caps().Has(protocol.CapUIDPlus) {
			return errors.New("COPY/MOVE requires UIDPLUS")
		}
		if move && !c.client.Caps().Has(protocol.CapMove) {
			return errors.New("MOVE extension required; unsafe EXPUNGE fallback disabled")
		}
		if e := c.selectRef(r); e != nil {
			return e
		}
		var validity uint32
		var nums protocol.NumSet
		var sources protocol.NumSet
		if move {
			data, e := c.client.Move(uid(r.UID), dest).Wait()
			if e != nil {
				return e
			}
			validity = data.UIDValidity
			nums = data.DestUIDs
			sources = data.SourceUIDs
		} else {
			data, e := c.client.Copy(uid(r.UID), dest).Wait()
			if e != nil {
				return e
			}
			validity = data.UIDValidity
			nums = data.DestUIDs
			sources = data.SourceUIDs
		}
		sourceSet, valid := sources.(protocol.UIDSet)
		if !valid {
			return errors.New("server omitted source UID mapping")
		}
		sourceUIDs, valid := sourceSet.Nums()
		if !valid || len(sourceUIDs) != 1 || uint32(sourceUIDs[0]) != r.UID {
			return errors.New("server returned mismatched source UID mapping")
		}
		set, ok := nums.(protocol.UIDSet)
		if !ok {
			return errors.New("server did not return destination UID mapping")
		}
		uids, ok := set.Nums()
		if !ok || len(uids) != 1 {
			return errors.New("server returned invalid destination UID mapping")
		}
		result = model.Ref{Folder: dest, Validity: validity, UID: uint32(uids[0])}
		if !result.Valid() {
			return errors.New("invalid destination UID")
		}
		return nil
	})
	return
}

var ErrNoIdle = errors.New("server does not advertise IDLE; periodic reconciliation remains active")

// Watch uses a dedicated connection. The caller reconnects with backoff and
// schedules a complete reconciliation after any gap in notifications.
func (c *Client) Watch(ctx context.Context, folder string) error {
	if !c.client.Caps().Has(protocol.CapIdle) {
		return ErrNoIdle
	}
	if err := c.run(ctx, func() error {
		_, e := c.client.Select(folder, &protocol.SelectOptions{ReadOnly: true}).Wait()
		return e
	}); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	cmd, err := c.client.Idle()
	if err != nil {
		return err
	}
	defer cmd.Close()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.client.Closed():
		return errors.New("IMAP IDLE connection closed")
	}
}

func (c *Client) Metadata(ctx context.Context, r model.Ref) (result model.Message, err error) {
	err = c.run(ctx, func() error {
		if e := c.selectRef(r); e != nil {
			return e
		}
		messages, e := c.client.Fetch(uid(r.UID), &protocol.FetchOptions{UID: true, Flags: true, InternalDate: true, RFC822Size: true}).Collect()
		if e != nil {
			return e
		}
		if len(messages) != 1 || uint32(messages[0].UID) != r.UID {
			return errors.New("remote membership disappeared")
		}
		m := messages[0]
		result = model.Message{Ref: r, Flags: decode(m.Flags), Date: m.InternalDate, Size: m.RFC822Size}
		return nil
	})
	return
}

// CheckOperation is a side-effect-free preflight. Unsupported capabilities
// must not turn an operation which was never sent into an uncertain request.
func (c *Client) CheckOperation(ctx context.Context, op model.Operation) error {
	if !c.writes {
		return errors.New("remote writes disabled")
	}
	if !c.client.Caps().Has(protocol.CapUIDPlus) {
		return errors.New("remote message mutations require UIDPLUS")
	}
	if op.Kind == "move" && !c.client.Caps().Has(protocol.CapMove) {
		return errors.New("MOVE extension required; EXPUNGE fallback disabled")
	}
	if op.Kind != "append" {
		_, err := c.Metadata(ctx, op.Source)
		return err
	}
	return ctx.Err()
}

// ListFolders includes subscribed and unsubscribed selectable mailboxes.
func (c *Client) ListFolders(ctx context.Context) (folders []string, err error) {
	err = c.run(ctx, func() error {
		boxes, e := c.client.List("", "*", nil).Collect()
		if e != nil {
			return e
		}
		set := map[string]bool{}
		for _, box := range boxes {
			selectable := true
			for _, attr := range box.Attrs {
				if strings.EqualFold(string(attr), string(protocol.MailboxAttrNoSelect)) || strings.EqualFold(string(attr), string(protocol.MailboxAttrNonExistent)) {
					selectable = false
				}
			}
			if !selectable {
				continue
			}
			if e = config.Folder(box.Mailbox); e != nil {
				return fmt.Errorf("discovered mailbox cannot be represented locally: %w", e)
			}
			set[box.Mailbox] = true
		}
		folders = model.SortedFolders(set)
		return nil
	})
	return
}

// RenameFolder moves the namespace atomically on the server, including inferior
// mailboxes on '/' hierarchy servers. Never rename INBOX: IMAP gives it special
// move-and-recreate semantics rather than an ordinary folder rename.
func (c *Client) RenameFolder(ctx context.Context, source, destination string) error {
	if !c.writes {
		return &model.FolderRejectedError{Err: errors.New("remote writes disabled")}
	}
	if strings.EqualFold(source, "INBOX") || strings.EqualFold(destination, "INBOX") {
		return &model.FolderRejectedError{Err: errors.New("INBOX cannot be renamed")}
	}
	if err := config.Folder(source); err != nil {
		return err
	}
	if err := config.Folder(destination); err != nil {
		return err
	}
	if source == destination || strings.HasPrefix(destination, source+"/") {
		return errors.New("invalid folder rename destination")
	}
	sent := false
	err := c.run(ctx, func() error {
		boxes, err := c.client.List("", "*", nil).Collect()
		if err != nil {
			return err
		}
		found := false
		for _, box := range boxes {
			if box.Mailbox == destination || strings.HasPrefix(box.Mailbox, destination+"/") {
				return errors.New("folder rename destination already exists")
			}
			if box.Mailbox == source {
				found = true
			}
			if box.Mailbox == source || strings.HasPrefix(box.Mailbox, source+"/") {
				if box.Delim != 0 && box.Delim != '/' {
					return errors.New("folder rename requires a '/' hierarchy delimiter")
				}
			}
		}
		if !found {
			return errors.New("folder rename source is missing")
		}
		sent = true
		return c.client.Rename(source, destination, nil).Wait()
	})
	return folderMutationError(err, sent)
}

func (c *Client) DeleteFolder(ctx context.Context, name string) error {
	if !c.writes {
		return &model.FolderRejectedError{Err: errors.New("remote writes disabled")}
	}
	if strings.EqualFold(name, "INBOX") {
		return &model.FolderRejectedError{Err: errors.New("INBOX cannot be removed")}
	}
	if err := config.Folder(name); err != nil {
		return &model.FolderRejectedError{Err: err}
	}
	sent := false
	err := c.run(ctx, func() error {
		// DELETE itself never expunges via a separate command. Servers requiring an
		// empty mailbox return NO, which is a known rejection rather than uncertainty.
		sent = true
		return c.client.Delete(name).Wait()
	})
	return folderMutationError(err, sent)
}

func folderMutationError(err error, sent bool) error {
	if err == nil {
		return nil
	}
	var response *protocol.Error
	if !sent || (errors.As(err, &response) && (response.Type == protocol.StatusResponseTypeNo || response.Type == protocol.StatusResponseTypeBad)) {
		return &model.FolderRejectedError{Err: err}
	}
	return err
}

// DeleteMessage uses UID EXPUNGE, never EXPUNGE or CLOSE: other clients may
// already have unrelated messages marked \Deleted in this mailbox.
func (c *Client) DeleteMessage(ctx context.Context, ref model.Ref, expected model.Flags) error {
	if !ref.Valid() {
		return errors.New("invalid deletion reference")
	}
	if !c.writes {
		return errors.New("remote writes disabled")
	}
	return c.run(ctx, func() error {
		if !c.client.Caps().Has(protocol.CapUIDPlus) {
			return errors.New("message deletion requires UIDPLUS; mailbox-wide EXPUNGE disabled")
		}
		if err := c.selectRef(ref); err != nil {
			return err
		}
		messages, err := c.client.Fetch(uid(ref.UID), &protocol.FetchOptions{UID: true, Flags: true}).Collect()
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			return nil
		} // A retry after a lost acknowledgement.
		if len(messages) != 1 || uint32(messages[0].UID) != ref.UID {
			return errors.New("invalid deletion UID response")
		}
		if decode(messages[0].Flags) != expected {
			return errors.New("remote flags changed before deletion")
		}
		if err = c.client.Store(uid(ref.UID), &protocol.StoreFlags{Op: protocol.StoreFlagsAdd, Silent: true, Flags: []protocol.Flag{protocol.FlagDeleted}}, nil).Close(); err != nil {
			return err
		}
		return c.client.UIDExpunge(uid(ref.UID)).Close()
	})
}
