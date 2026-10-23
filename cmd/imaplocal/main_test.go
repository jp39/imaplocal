package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/reconcile"
)

func TestCheckIsOfflineAndDoesNotInitializeState(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "credential-executed")
	c := config.Config{Server: "127.0.0.1:1", Username: "test", CredentialCommand: []string{"/bin/sh", "-c", "touch " + marker}, Maildir: filepath.Join(root, "mail"), Folders: []string{"INBOX"}}
	raw, _ := json.Marshal(c)
	path := filepath.Join(root, "config.json")
	os.WriteFile(path, raw, 0600)
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"check", "-config", path}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{marker, c.Maildir, c.StateDir()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("check created or accessed runtime state: %s", p)
		}
	}
}

func TestStatusIsPlainTextAndNeverExecutesCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	marker := filepath.Join(root, "credential-executed")
	c := config.Config{Server: "127.0.0.1:1", Username: "test", CredentialCommand: []string{"/bin/sh", "-c", "touch " + marker}, Maildir: filepath.Join(root, "mail"), Folders: []string{"INBOX"}}
	raw, _ := json.Marshal(c)
	path := config.DefaultPaths()[0]
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, raw, 0600)
	st, err := state.Open(c.StateDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.Put("issue", "entry", model.Issue{Kind: "writes_disabled", Detail: "local flags differ; remote_writes is false"}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err = run(context.Background(), []string{"status"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "Mode: read-only") || !strings.Contains(out.String(), "1 local flag change blocked: remote writes disabled") {
		t.Fatal("status is not informative plain text", out.String())
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("status ran credential command")
	}
}

func TestFilterPreviewIsOfflineAndApplyRequiresWriterLock(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "credentials-ran")
	script := filepath.Join(root, "rules.sieve")
	if err := os.WriteFile(script, []byte(`keep;`), 0600); err != nil {
		t.Fatal(err)
	}
	c := config.Config{Server: "127.0.0.1:1", Username: "test", CredentialCommand: []string{"/bin/sh", "-c", "touch " + marker}, Maildir: filepath.Join(root, "mail"), Folders: []string{"INBOX"}, FilterFolders: []string{"INBOX"}, RemoteWrites: true, Sieve: script}
	engine, err := reconcile.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	var out bytes.Buffer
	if err = filterCommand(context.Background(), c, reconcile.FilterOptions{}, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Dry run: 0 matched") {
		t.Fatal(out.String())
	}
	if err = filterCommand(context.Background(), c, reconcile.FilterOptions{}, true, &out); err == nil {
		t.Fatal("apply bypassed writer lock")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("preview/locked apply executed credentials")
	}
}

func TestDefaultConfigAndExplicitOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	base := filepath.Join(home, ".config", "imaplocal")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, "credentials-ran")
	c := config.Config{Server: "127.0.0.1:1", Username: "test", CredentialCommand: []string{"/bin/sh", "-c", "touch " + marker}, Maildir: "mail", Folders: []string{"INBOX"}, Sieve: "rules.sieve"}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "rules.sieve"), []byte("keep;"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"check"}, &out, &errOut); err != nil {
		t.Fatal("default check failed", err)
	}
	for _, target := range []string{marker, filepath.Join(base, "mail")} {
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("default check created runtime files", target, err)
		}
	}
	// A malformed higher-priority candidate is not permission to switch to
	// the valid account below it. An explicit override must still work.
	first := config.DefaultPaths()[0]
	if err := os.MkdirAll(filepath.Dir(first), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"check"}, &out, &errOut); err == nil || !strings.Contains(err.Error(), first) {
		t.Fatal("invalid first configuration was skipped", err)
	}
	if err := run(context.Background(), []string{"check", "-config", path}, &out, &errOut); err != nil {
		t.Fatal("explicit override did not bypass defaults", err)
	}
	missing := filepath.Join(home, "missing.json")
	if err := run(context.Background(), []string{"check", "-config", missing}, &out, &errOut); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatal("explicit missing configuration fell back", err)
	}
	if err := run(context.Background(), []string{"check", "-config", ""}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatal("explicit empty configuration fell back", err)
	}
}

func TestHelpWithoutConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, args := range [][]string{{"help"}, {"check", "-h"}, {"-version"}} {
		var out, errOut bytes.Buffer
		if err := run(context.Background(), args, &out, &errOut); err != nil {
			t.Fatal("help/version tried config discovery", args, err)
		}
	}
}
