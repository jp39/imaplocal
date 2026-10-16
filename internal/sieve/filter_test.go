package sieve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jp39/imaplocal/internal/model"
)

func TestRuleOrderFlagsAndCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "filter.sieve")
	script := `require ["fileinto", "copy", "imap4flags"];
if header :contains ["to", "cc"] "reviews@example.com" {
 addflag "\\Seen";
 fileinto :copy "INBOX/Review/Mine";
 fileinto "INBOX/Review";
 stop;
}
fileinto "Wrong";`
	os.WriteFile(path, []byte(script), 0600)
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: someone@example.com\r\nTo: reviews@example.com\r\n\r\nbody")
	targets, err := f.Evaluate(context.Background(), "INBOX", raw, model.Flagged)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].Folder != "INBOX/Review/Mine" || targets[1].Folder != "INBOX/Review" {
		t.Fatalf("wrong filing: %+v", targets)
	}
	for _, target := range targets {
		if target.Flags != model.Flagged|model.Seen {
			t.Fatalf("lost flags: %+v", target)
		}
	}
}
func TestUnsafeOrUnavailableActions(t *testing.T) {
	for _, script := range []string{`discard;`, `redirect "elsewhere@example.com";`, `require ["fileinto"]; fileinto "../outside";`, `require ["imap4flags"]; addflag "\\Deleted"; keep;`} {
		path := filepath.Join(t.TempDir(), "filter.sieve")
		os.WriteFile(path, []byte(script), 0600)
		f, err := Load(path)
		if err != nil {
			continue
		}
		if _, err = f.Evaluate(context.Background(), "INBOX", []byte("From: a@example.com\r\n\r\nbody"), 0); err == nil {
			t.Fatalf("unsafe action accepted: %s", script)
		}
	}
	path := filepath.Join(t.TempDir(), "filter.sieve")
	os.WriteFile(path, []byte(`require ["envelope"]; if envelope :contains "to" "x" { keep; }`), 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("invented SMTP envelope capability")
	}
}

func TestMalformedHeaderDoesNotHideRuleInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "filter.sieve")
	if err := os.WriteFile(path, []byte(`require "fileinto"; if header :contains "to" "reviews@example.com" { fileinto "Review"; }`), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.com\r\n${Subject}:\r\n To: fake@example.com\r\nTo: reviews@example.com\r\n\r\nbody")
	targets, err := f.Evaluate(context.Background(), "INBOX", raw, model.Seen)
	if err != nil || len(targets) != 1 || targets[0].Folder != "Review" || targets[0].Flags != model.Seen {
		t.Fatalf("valid headers not evaluated: %+v %v", targets, err)
	}
}
