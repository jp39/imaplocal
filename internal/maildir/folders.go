package maildir

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jp39/imaplocal/internal/model"
)

// Folder markers travel with external directory renames.
const folderMarker = ".imaplocal-folder"

// TrackFolder binds an empty or populated Maildir to a stable identity. The
// marker travels with an external directory rename, including empty folders.
func (s *Store) TrackFolder(name, id string) error {
	if err := s.Ensure(name); err != nil {
		return err
	}
	if _, err := hex.DecodeString(id); err != nil {
		return errors.New("invalid folder identity")
	}
	if len(id) != 32 {
		return errors.New("invalid folder identity")
	}
	folder, err := s.Folder(name)
	if err != nil {
		return err
	}
	path := filepath.Join(folder, folderMarker)
	expected := s.owner + "\n" + id + "\n"
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("folder identity is not a regular file")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(raw) != expected {
			return errors.New("folder identity belongs to another folder or account")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(folder, ".imaplocal-folder-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(expected); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(folder)
}

func (s *Store) FolderLocations() (map[string]string, error) {
	if err := s.checkOwner(); err != nil {
		return nil, err
	}
	out := map[string]string{}
	err := filepath.WalkDir(s.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in managed folder tree")
		}
		if d.IsDir() {
			if path != s.Root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "new" || d.Name() == "cur" || d.Name() == "tmp") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != folderMarker {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		parts := strings.Split(string(raw), "\n")
		if len(parts) != 3 || parts[0] != s.owner || len(parts[1]) != 32 || parts[2] != "" {
			return errors.New("invalid folder identity marker")
		}
		if _, err := hex.DecodeString(parts[1]); err != nil {
			return errors.New("invalid folder identity marker")
		}

		relative, err := filepath.Rel(s.Root, filepath.Dir(path))
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if old, duplicate := out[parts[1]]; duplicate {
			return fmt.Errorf("folder identity copied into both %s and %s", old, name)
		}
		out[parts[1]] = name
		return nil
	})
	return out, err
}

// RemoveFolderCore deletes only the observed message paths and this mailbox's
// standard directories. New or unrecognized files make rmdir fail; inferior
// mailboxes remain intact. Each step can resume after a process interruption.
func (s *Store) RemoveFolderCore(name string, observed []model.Local) error {
	if err := s.checkOwner(); err != nil {
		return err
	}
	folder, err := s.Folder(name)
	if err != nil {
		return err
	}
	temporary, err := os.ReadDir(filepath.Join(folder, "tmp"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(temporary) > 0 {
		return errors.New("folder has in-progress temporary messages")
	}
	for _, local := range observed {
		if local.Folder != name {
			continue
		}
		info, err := os.Lstat(local.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != local.Size || info.ModTime().UnixNano() != local.ModTime {
			return errors.New("local message changed during folder removal")
		}
		if err = os.Remove(local.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, base := range []string{"new", "cur", "tmp"} {
		dir := filepath.Join(folder, base)
		if err = os.Remove(dir); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	marker := filepath.Join(folder, folderMarker)
	if err = os.Remove(marker); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err = os.Stat(folder); err == nil {
		if err = syncDir(folder); err != nil {
			return err
		}
	}
	if err = os.Remove(folder); err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	if _, err = os.Stat(filepath.Dir(folder)); err == nil {
		return syncDir(filepath.Dir(folder))
	}
	return nil
}
