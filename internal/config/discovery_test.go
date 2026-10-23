package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDefaultPathsOrderAndDeduplication(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	native := filepath.Join(home, "native")
	config := filepath.Join(home, ".config", "imaplocal", "config.json")
	dotfile := filepath.Join(home, ".imaplocal.json")
	for _, tc := range []struct {
		name, xdg, native string
		want              []string
	}{
		{"all", xdg, native, []string{filepath.Join(xdg, "imaplocal", "config.json"), config, filepath.Join(native, "imaplocal", "config.json"), dotfile}},
		{"relative XDG ignored", "relative", native, []string{config, filepath.Join(native, "imaplocal", "config.json"), dotfile}},
		{"duplicate native", "", filepath.Join(home, ".config"), []string{config, dotfile}},
		{"duplicate XDG", filepath.Join(home, ".config"), filepath.Join(home, ".config"), []string{config, dotfile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultPaths(home, tc.native, tc.xdg); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFindDefaultSearchesInOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	paths := DefaultPaths()
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range paths {
		got, err := FindDefault()
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
		if err := os.Remove(want); err != nil {
			t.Fatal(err)
		}
	}
	_, err := FindDefault()
	if err == nil || !strings.Contains(err.Error(), "-config FILE") {
		t.Fatal("missing configuration error lacks explicit override guidance", err)
	}
	for _, path := range paths {
		if !strings.Contains(err.Error(), path) {
			t.Fatal("missing configuration error omits search location", path, err)
		}
	}
}

func TestFindDefaultHandlesSymlinksAndRejectsBrokenCandidates(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
			first := DefaultPaths()[0]
			if err := os.MkdirAll(filepath.Dir(first), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".imaplocal.json"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(home, "account.json")
			if kind == "directory" {
				if err := os.Mkdir(first, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if kind == "symlink" {
					if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, first); err != nil {
					t.Fatal(err)
				}
			}
			got, err := FindDefault()
			if kind == "symlink" {
				if err != nil || got != first {
					t.Fatal("valid symlink not selected", got, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), first) {
				t.Fatal("bad first candidate silently fell back", got, err)
			}
		})
	}
}
