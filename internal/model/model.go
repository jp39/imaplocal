package model

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// These failures describe successful IMAP commands without a usable body.
// Transport/protocol errors must not be treated as missing messages.
var (
	ErrRemoteMessageGone     = errors.New("remote message no longer exists")
	ErrRemoteBodyUnavailable = errors.New("remote message exists but its body is unavailable")
)

// Flags contains the portable flags imaplocal can faithfully represent in Maildir.
// Deleted is intentionally excluded: disappearance is reconciled separately.
type Flags uint8

const (
	Seen Flags = 1 << iota
	Answered
	Flagged
	Draft
	Forwarded
)
const AllFlags = Seen | Answered | Flagged | Draft | Forwarded

func (f Flags) Maildir() string {
	var b strings.Builder
	for _, p := range []struct {
		flag Flags
		char byte
	}{{Draft, 'D'}, {Flagged, 'F'}, {Forwarded, 'P'}, {Answered, 'R'}, {Seen, 'S'}} {
		if f&p.flag != 0 {
			b.WriteByte(p.char)
		}
	}
	return b.String()
}
func ParseFlags(s string) (Flags, error) {
	var f Flags
	for _, c := range s {
		switch c {
		case 'S':
			f |= Seen
		case 'R':
			f |= Answered
		case 'F':
			f |= Flagged
		case 'D':
			f |= Draft
		case 'P':
			f |= Forwarded
		case 'T':
			return 0, fmt.Errorf("Maildir deletion flag requires an explicit deletion policy")
		default:
			return 0, fmt.Errorf("unsupported Maildir flag %q", c)
		}
	}
	return f, nil
}
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type Ref struct {
	Folder   string
	Validity uint32
	UID      uint32
}

func (r Ref) Key() string { return fmt.Sprintf("%s\x00%d\x00%d", r.Folder, r.Validity, r.UID) }
func (r Ref) Valid() bool { return r.Folder != "" && r.Validity != 0 && r.UID != 0 }

type Message struct {
	Ref   Ref
	Flags Flags
	Date  time.Time
	Size  int64
}
type Mailbox struct {
	Folder   string
	Validity uint32
	NextUID  uint32
	Messages []Message
}
type Local struct {
	Deleted bool
	ID      string
	Folder  string
	Path    string
	Flags   Flags
	Size    int64
	ModTime int64
	Key     string
}
type Entry struct {
	// ReceivedAt is the IMAP INTERNALDATE, independent of sender headers and
	// filesystem timestamps changed by external tools.
	ReceivedAt       time.Time
	DeletionConflict bool
	FolderRemoved    bool
	ID               string
	Remote           Ref
	Agreed           Flags
	Hash             string
	Size             int64
	ModTime          int64
	Tombstone        bool
}
type Delivery struct {
	Date   time.Time
	ID     string
	Folder string
	Flags  Flags
	Remote Ref
}
type Arrival struct {
	// OriginID identifies the mapped local membership superseded by an explicit
	// manual filtering plan. Ordinary first-time arrivals leave it empty.
	OriginID   string
	Cleaned    bool
	ID         string
	Source     Message
	Hash       string
	RuleHash   string
	Deliveries []Delivery
	Done       bool
}
type Operation struct {
	LocalFlags  Flags
	RemoteFlags Flags
	Cleaned     bool
	ArrivalID   string
	OriginID    string
	Applied     bool
	Verified    bool
	ID          string
	Kind        string
	Source      Ref
	Destination string
	EntryID     string
	Hash        string
	Flags       Flags
	Date        time.Time
	Phase       string // planned, inflight, unknown, done
	Result      Ref
	Error       string
}
type Issue struct {
	ID      string
	Kind    string
	Detail  string
	Updated time.Time
}
type Boundary struct {
	Validity uint32
	NextUID  uint32
}

// Backend operations returning an error may have had remote effects. The
// operation journal, rather than the adapter, decides whether retry is safe.
type Backend interface {
	Snapshot(context.Context, string) (Mailbox, error)
	Metadata(context.Context, Ref) (Message, error)
	Fetch(context.Context, Ref, int64) ([]byte, error)
	Flags(context.Context, Ref, Flags, Flags) error // add, remove; preserve other flags
	Append(context.Context, string, []byte, Flags, time.Time) (Ref, error)
	Transfer(context.Context, Ref, string, bool) (Ref, error) // move iff final argument
	Ensure(context.Context, string) error
	Close() error
}

func SortedFolders(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func (o Operation) IDSafe() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(o.ID)))
}

// FolderLister returns a complete successful observation of selectable mailboxes.
// A failure is never interpreted as an empty folder set.
type FolderLister interface {
	ListFolders(context.Context) ([]string, error)
}

type Folder struct {
	Validity   uint32
	ID         string
	Name       string
	Retired    bool
	ReplacedBy string
}

type FolderMapping struct {
	From     string
	To       string
	ID       string
	Validity uint32
	NextUID  uint32
}

// FolderOperation journals namespace mutations separately from message effects.
// remote_done means the server acknowledged the rename; unknown is never retried.
type FolderOperation struct {
	ID          string
	Kind        string // rename, remove, retire (remote disappearance)
	Source      string
	Destination string
	Phase       string // planned, inflight, unknown, remote_done, done
	Mappings    []FolderMapping
	Error       string
}

type FolderManager interface {
	RenameFolder(context.Context, string, string) error
	DeleteFolder(context.Context, string) error
}

// FolderRejectedError proves a mutation was not sent or was rejected by a tagged
// server response. Transport errors after sending retain an unknown outcome.
type FolderRejectedError struct{ Err error }

func (e *FolderRejectedError) Error() string { return e.Err.Error() }
func (e *FolderRejectedError) Unwrap() error { return e.Err }

// MessageDeleter permanently removes only the supplied UID. Repeating a request
// for the same UID is safe within its UIDVALIDITY epoch. expected protects
// against observed concurrent portable flag changes.
type MessageDeleter interface {
	DeleteMessage(context.Context, Ref, Flags) error
}

// BodySink receives one complete, UID-verified body or a confirmed per-message
// absence/unavailability error. It may persist checkpoints but must not issue
// remote commands: a streaming FETCH may still be active on that connection.
// Returning an error aborts the batch. No callback is made for partial bodies.
type BodySink func(Ref, []byte, error) error

// BatchFetcher streams a single mailbox/UIDVALIDITY group without retaining
// all bodies in memory. A transport failure may follow successful callbacks;
// the caller must preserve their durable checkpoints for the next connection.
type BatchFetcher interface {
	FetchBatch(context.Context, []Ref, int64, BodySink) error
}

const MaxFetchBatchMessages = 64

// BodyTooLargeError reports a known literal size without buffering its body.
type BodyTooLargeError struct{ Size, Limit int64 }

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("body size %d exceeds max_message_bytes %d", e.Size, e.Limit)
}

// BodySize remembers an observed MIME size when RFC822.SIZE understated it.
// Its UID-scoped identity lets a later cycle skip the body until the limit rises.
type BodySize struct {
	Ref  Ref
	Size int64
}
