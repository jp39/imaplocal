package maildir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/model"
)

// ErrChanged means a message path changed during observation (usually an MUA
// flag rename). Discard the observation and rescan; never infer deletion from it.
var ErrChanged = errors.New("Maildir changed during observation")

func observationError(err error) error {
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %w", ErrChanged, err)
	}
	return err
}

const messagePrefix = "imaplocal-"

type Store struct {
	Root    string
	Private string
	owner   string
}

// Inspect opens an existing owned Maildir without creating markers or staging.
func Inspect(root, private, owner string) (*Store, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	s := &Store{Root: real, Private: private, owner: owner}
	if err = s.checkOwner(); err != nil {
		return nil, err
	}
	return s, nil
}

func Hash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func New(root, private, owner string) (*Store, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	s := &Store{Root: real, Private: private, owner: owner}
	if err := os.MkdirAll(private, 0700); err != nil {
		return nil, err
	}
	marker := filepath.Join(private, "owner")
	b, err := os.ReadFile(marker)
	if err == nil {
		if string(b) != owner {
			return nil, errors.New("Maildir belongs to a different imaplocal state directory")
		}
	} else if os.IsNotExist(err) {
		entries, e := os.ReadDir(real)
		if e != nil {
			return nil, e
		}
		// Opening state creates the private directory before the first Maildir
		// ownership marker. No user mail may be adopted alongside it.
		if len(entries) != 0 && !(len(entries) == 1 &&
			entries[0].Name() == config.StateDirectory && entries[0].IsDir() &&
			filepath.Clean(private) == filepath.Join(real, config.StateDirectory)) {
			return nil, errors.New("refusing to adopt a nonempty Maildir; use a fresh isolated directory")
		}
		f, e := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.WriteString(owner)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return nil, e
		}
		if ce != nil {
			return nil, ce
		}
		if e = syncDir(private); e != nil {
			return nil, e
		}
	} else {
		return nil, err
	}
	for _, d := range []string{"staging", "quarantine"} {
		if err = os.MkdirAll(filepath.Join(private, d), 0700); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) checkOwner() error {
	b, e := os.ReadFile(filepath.Join(s.Private, "owner"))
	if e != nil {
		return e
	}
	if string(b) != s.owner {
		return errors.New("Maildir ownership marker changed")
	}
	return nil
}
func (s *Store) Folder(folder string) (string, error) {
	if err := config.Folder(folder); err != nil {
		return "", err
	}
	path := s.Root
	for _, part := range strings.Split(folder, "/") {
		path = filepath.Join(path, part)
		info, e := os.Lstat(path)
		if e == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return "", errors.New("Maildir contains a symlink or non-directory")
		}
		if e != nil && !os.IsNotExist(e) {
			return "", e
		}
	}
	return path, nil
}
func (s *Store) Ensure(folder string) error {
	if err := s.checkOwner(); err != nil {
		return err
	}
	p, err := s.Folder(folder)
	if err != nil {
		return err
	}
	for _, sub := range []string{"tmp", "new", "cur"} {
		target := filepath.Join(p, sub)
		if err = os.MkdirAll(target, 0700); err != nil {
			return err
		}
		info, e := os.Lstat(target)
		if e != nil {
			return e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Maildir subdirectory is not a real directory")
		}
	}
	return nil
}

// Scan is a complete observation. Any unreadable directory invalidates the scan;
// callers must never interpret that failure as evidence of message deletion.
func (s *Store) Scan() ([]model.Local, error) {
	if err := s.checkOwner(); err != nil {
		return nil, err
	}
	out := []model.Local{}
	err := filepath.WalkDir(s.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == s.Root {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in managed Maildir: %s", path)
		}
		if !d.IsDir() {
			if d.Name() == ".imaplocal-folder" {
				folder, err := filepath.Rel(s.Root, filepath.Dir(path))
				if err != nil {
					return err
				}
				return s.CheckFolder(filepath.ToSlash(folder))
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.Name() == "tmp" {
			return filepath.SkipDir
		}
		if d.Name() != "cur" && d.Name() != "new" {
			return nil
		}
		folder, err := filepath.Rel(s.Root, filepath.Dir(path))
		if err != nil {
			return err
		}
		folder = filepath.ToSlash(folder)
		if err = config.Folder(folder); err != nil {
			return err
		}
		if err = s.CheckFolder(folder); err != nil {
			return err
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			info, e := entry.Info()
			if e != nil {
				return observationError(e)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("non-regular message in %s", path)
			}
			key, suffix, found := strings.Cut(entry.Name(), ":2,")
			if !found && strings.Contains(entry.Name(), ":") {
				return fmt.Errorf("invalid Maildir filename: %s", entry.Name())
			}
			flags, e := model.ParseFlags(strings.ReplaceAll(suffix, "T", ""))
			if e != nil {
				return fmt.Errorf("%s: %w", entry.Name(), e)
			}
			id := ""
			if strings.HasPrefix(key, messagePrefix) && len(key) == len(messagePrefix)+32 {
				if _, e = hex.DecodeString(key[len(messagePrefix):]); e == nil {
					id = key[len(messagePrefix):]
				}
			}
			out = append(out, model.Local{Deleted: strings.Contains(suffix, "T"), ID: id, Key: key, Folder: folder, Path: filepath.Join(path, entry.Name()), Flags: flags, Size: info.Size(), ModTime: info.ModTime().UnixNano()})
		}
		return filepath.SkipDir
	})
	return out, err
}
func Read(local model.Local, max int64) ([]byte, error) {
	info, err := os.Lstat(local.Path)
	if err != nil {
		return nil, observationError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("message is not a regular file")
	}
	if info.Size() > max {
		return nil, errors.New("message exceeds max_message_bytes")
	}
	f, err := os.Open(local.Path)
	if err != nil {
		return nil, observationError(err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, observationError(err)
	}
	after, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > max || after.Size() != info.Size() || after.ModTime() != info.ModTime() {
		return nil, errors.New("message changed while reading or exceeds size limit")
	}
	return b, nil
}
func (s *Store) Stage(id string, raw []byte) error {
	return writePrivate(filepath.Join(s.Private, "staging", id), raw)
}
func (s *Store) Staged(id string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.Private, "staging", id))
}
func writePrivate(path string, raw []byte) error {
	if old, e := os.ReadFile(path); e == nil {
		if Hash(old) != Hash(raw) {
			return errors.New("staged content identity mismatch")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".stage-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(raw); err == nil {
		err = temp.Sync()
	}
	ce := temp.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err = os.Link(temp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// Publish uses an application-owned stable key. The supplied complete index
// recognizes publication before a previous process crashed, including MUA moves.
func (s *Store) Publish(d model.Delivery, raw []byte, index map[string][]model.Local) (model.Local, error) {
	if matches := index[d.ID]; len(matches) != 0 {
		if len(matches) != 1 {
			return model.Local{}, errors.New("ambiguous existing publication")
		}
		l := matches[0]
		if l.Folder != d.Folder {
			return l, errors.New("message moved during publication recovery")
		}
		b, err := Read(l, int64(len(raw))+1)
		if err != nil {
			return l, err
		}
		if Hash(b) != Hash(raw) {
			return l, errors.New("published message was edited")
		}
		return l, nil
	}
	if err := s.Ensure(d.Folder); err != nil {
		return model.Local{}, err
	}
	folder, _ := s.Folder(d.Folder)
	tmp, err := os.CreateTemp(filepath.Join(folder, "tmp"), messagePrefix)
	if err != nil {
		return model.Local{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(raw); err == nil && !d.Date.IsZero() {
		err = os.Chtimes(tmp.Name(), d.Date, d.Date)
	}
	if err == nil {
		err = tmp.Sync()
	}
	ce := tmp.Close()
	if err != nil {
		return model.Local{}, err
	}
	if ce != nil {
		return model.Local{}, ce
	}
	// Match mbsync's maildir_store_msg: Seen messages go straight into cur/;
	// unread messages go into new/, retaining their :2, flag suffix. Direct
	// cur/ publication deviates from the original tmp->new delivery protocol,
	// but avoids presenting already-read imports as new mail. It also keeps
	// their flags intact in readers such as aerc, whose new->cur acknowledgement
	// discards info suffixes. Unread messages with other flags retain that
	// reader-compatibility limitation; using any flag to select cur/ would
	// change mbsync's unread/new semantics.
	sub := "new"
	if d.Flags&model.Seen != 0 {
		sub = "cur"
	}
	name := messagePrefix + d.ID + ":2," + d.Flags.Maildir()
	path := filepath.Join(folder, sub, name)
	if err = os.Link(tmp.Name(), path); err != nil {
		return model.Local{}, err
	}
	if err = syncDir(filepath.Join(folder, sub)); err != nil {
		return model.Local{}, err
	}
	info, err := os.Stat(tmp.Name())
	if err != nil {
		return model.Local{}, err
	}
	return model.Local{ID: d.ID, Key: messagePrefix + d.ID, Folder: d.Folder, Path: path, Flags: d.Flags, Size: info.Size(), ModTime: info.ModTime().UnixNano()}, nil
}
func (s *Store) Rename(l model.Local, id string, flags model.Flags) (model.Local, error) {
	if err := s.checkOwner(); err != nil {
		return l, err
	}
	folder, err := s.Folder(l.Folder)
	if err != nil {
		return l, err
	}
	sub := filepath.Base(filepath.Dir(l.Path))
	if sub != "cur" && sub != "new" {
		return l, errors.New("message outside Maildir")
	}
	originalDir := filepath.Dir(l.Path)
	// Match mbsync's maildir_set_msg_flags: applying a flag update acknowledges
	// the message into cur/, even when the resulting flags do not include Seen.
	// Once acknowledged, clearing Seen must never make it new again.
	sub = "cur"
	name := messagePrefix + id + ":2," + flags.Maildir()
	path := filepath.Join(folder, sub, name)
	if path != l.Path {
		if _, err = os.Lstat(path); err == nil {
			return l, errors.New("rename would overwrite a message")
		}
		if err = os.Rename(l.Path, path); err != nil {
			return l, observationError(err)
		}
		if err = syncDir(filepath.Dir(path)); err != nil {
			return l, err
		}
		if originalDir != filepath.Dir(path) {
			if err = syncDir(originalDir); err != nil {
				return l, err
			}
		}
	}
	l.ID = id
	l.Key = messagePrefix + id
	l.Path = path
	l.Flags = flags
	return l, nil
}

// Unstage removes only a completed operation's private scratch copy.
func (s *Store) Unstage(id string) error {
	err := os.Remove(filepath.Join(s.Private, "staging", id))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Join(s.Private, "staging"))
}

// CheckFolder prevents an incomplete Maildir (for example a missing cur/)
// from turning missing-file observations into remote deletions.
func (s *Store) CheckFolder(name string) error {
	if err := s.checkOwner(); err != nil {
		return err
	}
	folder, err := s.Folder(name)
	if err != nil {
		return err
	}
	for _, sub := range []string{"new", "cur", "tmp"} {
		info, err := os.Lstat(filepath.Join(folder, sub))
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("incomplete Maildir")
		}
	}
	return nil
}

// Remove checks the bytes immediately before unlinking the observed path.
// A concurrent flag rename or move invalidates that path and leaves a conflict.
// It never removes another membership merely because its body is identical.
func (s *Store) Remove(l model.Local, hash string, max int64) error {
	if err := s.CheckFolder(l.Folder); err != nil {
		return err
	}
	folder, err := s.Folder(l.Folder)
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.Path)
	if dir != filepath.Join(folder, "cur") && dir != filepath.Join(folder, "new") {
		return errors.New("message outside managed Maildir")
	}
	raw, err := Read(l, max)
	if err != nil {
		return err
	}
	if Hash(raw) != hash {
		return errors.New("local message body changed before removal")
	}
	if err = os.Remove(l.Path); err != nil {
		return err
	}
	return syncDir(dir)
}
