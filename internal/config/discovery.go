package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPaths lists user configuration candidates in precedence order.
func DefaultPaths() []string {
	home, _ := os.UserHomeDir()
	native, _ := os.UserConfigDir()
	return defaultPaths(home, native, os.Getenv("XDG_CONFIG_HOME"))
}

func defaultPaths(home, native, xdg string) []string {
	var candidates []string
	if filepath.IsAbs(xdg) {
		candidates = append(candidates, filepath.Join(xdg, "imaplocal", "config.json"))
	}
	if filepath.IsAbs(home) {
		candidates = append(candidates, filepath.Join(home, ".config", "imaplocal", "config.json"))
	}
	if filepath.IsAbs(native) {
		candidates = append(candidates, filepath.Join(native, "imaplocal", "config.json"))
	}
	if filepath.IsAbs(home) {
		candidates = append(candidates, filepath.Join(home, ".imaplocal.json"))
	}
	seen := make(map[string]bool)
	var paths []string
	for _, path := range candidates {
		if !seen[path] {
			paths = append(paths, path)
			seen[path] = true
		}
	}
	return paths
}

// FindDefault selects the first existing candidate. Invalid or inaccessible
// files are errors, not permission to silently select a different account.
func FindDefault() (string, error) {
	paths := DefaultPaths()
	for _, path := range paths {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("inspect default configuration %q: %w", path, err)
		}
		// Follow symlinks, but report dangling ones instead of falling back.
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("inspect default configuration %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("default configuration %q is not a regular file", path)
		}
		return path, nil
	}
	return "", fmt.Errorf("no configuration file found (searched: %s); use -config FILE", strings.Join(paths, ", "))
}
