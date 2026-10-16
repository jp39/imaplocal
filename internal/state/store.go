package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	lock *os.File
}

func Open(dir string, readOnly bool) (*Store, error) {
	if !readOnly {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	// State belongs inside the Maildir. Do not follow a private-directory
	// symlink that could redirect its database, lock, or staged content.
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("state directory must be a real directory")
	}
	path := filepath.Join(dir, "state.sqlite")
	mode := "rwc"
	if readOnly {
		mode = "ro"
	}
	s := &Store{}
	if !readOnly {
		f, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			f.Close()
			return nil, errors.New("another imaplocal writer holds this state directory")
		}
		s.lock = f
	}
	// URI encoding is delegated to net/url to preserve literal ?, #, and spaces.
	db, err := openDB(path, mode)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.db = db
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		s.Close()
		return nil, err
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		s.Close()
		return nil, err
	}
	if version > 1 {
		s.Close()
		return nil, fmt.Errorf("unsupported database schema %d", version)
	}
	if !readOnly {
		if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS records (kind TEXT NOT NULL, id TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(kind,id)); PRAGMA user_version=1;`); err != nil {
			s.Close()
			return nil, err
		}
		if _, err = db.Exec(summaryIndexes); err != nil {
			s.Close()
			return nil, err
		}
		if err = os.Chmod(path, 0600); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
		s.db = nil
	}
	if s.lock != nil {
		_ = unix.Flock(int(s.lock.Fd()), unix.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
	return err
}
func (s *Store) Put(kind, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO records(kind,id,data) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data WHERE records.data != excluded.data`, kind, id, b)
	return err
}
func (s *Store) Get(kind, id string, v any) (bool, error) {
	var b []byte
	err := s.db.QueryRow(`SELECT data FROM records WHERE kind=? AND id=?`, kind, id).Scan(&b)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}
func List[T any](s *Store, kind string) ([]T, error) {
	rows, err := s.db.Query(`SELECT data FROM records WHERE kind=? ORDER BY id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var v T
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Delete(kind, id string) error {
	_, err := s.db.Exec(`DELETE FROM records WHERE kind=? AND id=?`, kind, id)
	return err
}

// Change applies a set of namespace metadata edits atomically. The external
// filesystem/server steps remain journaled separately.
type Change struct {
	Kind, ID string
	Value    any
	Delete   bool
}

func (s *Store) Apply(changes []Change) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, ch := range changes {
		if ch.Delete {
			_, err = tx.Exec(`DELETE FROM records WHERE kind=? AND id=?`, ch.Kind, ch.ID)
		} else {
			var raw []byte
			raw, err = json.Marshal(ch.Value)
			if err == nil {
				_, err = tx.Exec(`INSERT INTO records(kind,id,data) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data`, ch.Kind, ch.ID, raw)
			}
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Keys(kind string) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM records WHERE kind=? ORDER BY id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
