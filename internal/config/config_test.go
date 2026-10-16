package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPathsAndStrictConfiguration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	raw := `{"server":"localhost:993","username":"user","credential_command":["helper"],"maildir":"mail","folders":["INBOX"]}`
	os.WriteFile(path, []byte(raw), 0600)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Maildir != filepath.Join(root, "mail") || c.RemoteWrites || c.StateDir() != filepath.Join(root, "mail", StateDirectory) {
		t.Fatal("unsafe defaults or incorrect paths")
	}
	for _, modify := range []func(map[string]any){func(v map[string]any) { v["remote_write"] = true }, func(v map[string]any) { v["state_dir"] = "mail/state" }, func(v map[string]any) { v["folders"] = []string{"../escape"} }, func(v map[string]any) { v["interval"] = "0s" }, func(v map[string]any) { v["proxy_url"] = "socks5://localhost:1080" }, func(v map[string]any) { v["proxy_url"] = "http://user:secret@localhost:80" }, func(v map[string]any) { v["proxy_url"] = "http://localhost/path" }} {
		var v map[string]any
		json.Unmarshal([]byte(raw), &v)
		modify(v)
		b, _ := json.Marshal(v)
		os.WriteFile(path, b, 0600)
		if _, err = Load(path); err == nil {
			t.Fatal("invalid configuration accepted", string(b))
		}
	}
}

func TestWildcardSelectionValidation(t *testing.T) {
	root := t.TempDir()
	base := Config{Server: "localhost:993", Username: "test", CredentialCommand: []string{"helper"}, Maildir: filepath.Join(root, "mail")}
	for _, tc := range []struct {
		folders, filters []string
		valid            bool
	}{
		{[]string{"*"}, []string{"INBOX"}, true},
		{[]string{"*", "Archive"}, nil, true},
		{[]string{"INBOX/*"}, []string{"INBOX/Review"}, true},
		{[]string{"INBOX/*"}, []string{"INBOX"}, false},
		{[]string{"INBOX/%"}, []string{"INBOX/Review/My Reviews"}, false},
		{[]string{"INBOX/../*"}, nil, false},
		{[]string{"INBOX/*"}, nil, true},
		{[]string{"%"}, nil, true},
		{[]string{"*", "*"}, nil, false},
		{[]string{"*"}, []string{"*"}, false},
		{[]string{"*"}, []string{"../bad"}, false},
	} {
		c := base
		c.Folders = tc.folders
		c.FilterFolders = tc.filters
		if err := c.Validate(); (err == nil) != tc.valid {
			t.Fatalf("%v / %v: %v", tc.folders, tc.filters, err)
		}
	}
}

func TestFolderPatternMatching(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"INBOX/*", "INBOX/Review", true},
		{"INBOX/*", "INBOX/Review/My Reviews", true},
		{"INBOX/*", "INBOX", false},
		{"INBOX/*", "Calendar", false},
		{"INBOX/%", "INBOX/Review", true},
		{"INBOX/%", "INBOX/Review/My Reviews", false},
		{"%", "INBOX", true},
		{"%", "INBOX/Review", false},
		{"*/Review", "INBOX/Review", true},
		{"INBOX/Review*", "INBOX/Review/My Reviews", true},
		{"INBOX/[a].*", "INBOX/[a].literal", true},
		{"INBOX/[a].*", "INBOX/a.literal", false},
		{"INBOX/*", "INBOX/日本語", true},
	} {
		c := Config{Folders: []string{tc.pattern}}
		if got := c.SelectsFolder(tc.name); got != tc.want {
			t.Errorf("%q matches %q: got %v", tc.pattern, tc.name, got)
		}
	}
}
