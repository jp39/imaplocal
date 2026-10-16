package maildir

import (
	"path/filepath"
	"strings"

	"github.com/jp39/imaplocal/internal/config"
)

// Private database/staging writes must not schedule another reconciliation.
// FSEvents reports paths without their leading slash; inotify uses absolute paths.
func privateEvent(root, path string) bool {
	if !filepath.IsAbs(path) {
		path = string(filepath.Separator) + path
	}
	private := filepath.Join(root, config.StateDirectory)
	return path == private || strings.HasPrefix(path, private+string(filepath.Separator))
}
