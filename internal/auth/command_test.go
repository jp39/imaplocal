package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBoundedCredentialOutputAndRedactedErrors(t *testing.T) {
	for _, script := range []string{`printf 'first\nsecond\n'`, `printf 'secret-on-stderr' >&2; exit 1`, `head -c 20000 /dev/zero`} {
		c := Command{Args: []string{"/bin/sh", "-c", script}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := c.Credential(ctx)
		cancel()
		if err == nil {
			t.Fatal("bad credential output accepted")
		}
		if strings.Contains(err.Error(), "secret-on-stderr") {
			t.Fatal("credential stderr leaked")
		}
	}
	c := Command{Args: []string{"/usr/bin/printf", "token\r\n"}}
	got, err := c.Credential(context.Background())
	if err != nil || got != "token" {
		t.Fatalf("normal token: %q %v", got, err)
	}
}
