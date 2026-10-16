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
	marker := filepath.Join(root, "credential-executed")
	c := config.Config{Server: "127.0.0.1:1", Username: "test", CredentialCommand: []string{"/bin/sh", "-c", "touch " + marker}, Maildir: filepath.Join(root, "mail"), Folders: []string{"INBOX"}}
	raw, _ := json.Marshal(c)
	path := filepath.Join(root, "config.json")
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
	if err = run(context.Background(), []string{"status", "-config", path}, &out, &errOut); err != nil {
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
