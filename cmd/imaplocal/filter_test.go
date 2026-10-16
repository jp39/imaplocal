package main

import (
	"bytes"
	"context"
	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/reconcile"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilterCutoff(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 30, 0, 123, time.FixedZone("CEST", 2*60*60))
	localMidnight := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	for _, test := range []struct {
		since, within string
		want          time.Time
	}{
		{"", "", time.Time{}},
		{"2026-10-01", "", localMidnight},
		{"2026-10-01T00:00:00+02:00", "", time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)},
		{"", "10d", now.Add(-240 * time.Hour)},
		{"", "24h", now.Add(-24 * time.Hour)},
	} {
		got, err := filterCutoff(test.since, test.within, now)
		if err != nil || !got.Equal(test.want) {
			t.Fatalf("%q %q: %v %v, want %v", test.since, test.within, got, err, test.want)
		}
	}
	for _, test := range []struct{ since, within string }{
		{"2026-10-01", "10d"}, {"2026-02-30", ""}, {"yesterday", ""},
		{"2026-10-01T12:00:00", ""}, {"", "0d"}, {"", "-1d"}, {"", "1.5d"},
		{"", "999999999999999999d"}, {"", "0h"}, {"", "-24h"}, {"", "10"},
	} {
		if _, err := filterCutoff(test.since, test.within, now); err == nil {
			t.Fatalf("invalid cutoff accepted: %+v", test)
		}
	}
}

func TestFilterOutputIncludesReceivedDate(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "filter.sieve")
	if err := os.WriteFile(script, []byte(`require "fileinto"; fileinto "Filed";`), 0600); err != nil {
		t.Fatal(err)
	}
	c := config.Config{Server: "example.org:993", Username: "test", CredentialCommand: []string{"unused"}, Maildir: filepath.Join(root, "mail"), Folders: []string{"INBOX"}, FilterFolders: []string{"INBOX"}, Sieve: script}
	e, err := reconcile.Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	date := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	raw := []byte("Message-ID: <output@example.org>\r\n\r\nbody\r\n")
	id := model.NewID()
	local, err := e.Local.Publish(model.Delivery{ID: id, Folder: "INBOX", Date: date}, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry := model.Entry{ID: id, ReceivedAt: date, Remote: model.Ref{Folder: "INBOX", Validity: 1, UID: 1}, Hash: maildir.Hash(raw), Size: local.Size, ModTime: local.ModTime}
	if err = e.State.Put("entry", id, entry); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = filterCommand(context.Background(), c, reconcile.FilterOptions{Since: date.Add(-time.Hour)}, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "received="+date.In(time.Local).Format(time.RFC3339Nano)) || !strings.Contains(out.String(), "Received on/after:") || !strings.Contains(out.String(), "1 would change") {
		t.Fatal("missing date context", out.String())
	}
}
