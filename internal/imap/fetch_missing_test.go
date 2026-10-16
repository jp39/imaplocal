package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jp39/imaplocal/internal/model"
)

func TestFetchMissingBodyRequiresAbsenceConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, body, metadata string
		want                 error
	}{
		{"expunged", "", "", model.ErrRemoteMessageGone},
		{"existing-without-response", "", "* 1 FETCH (UID 1)\r\n", model.ErrRemoteBodyUnavailable},
		{"nil-body", "* 1 FETCH (UID 1 BODY[] NIL)\r\n", "* 1 FETCH (UID 1)\r\n", model.ErrRemoteBodyUnavailable},
		{"empty-body", "* 1 FETCH (UID 1 BODY[] {0}\r\n)\r\n", "", nil},
		{"metadata-failure", "", "NO metadata unavailable\r\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, server := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer server.Close()
				fmt.Fprint(server, "* PREAUTH [CAPABILITY IMAP4rev1] test\r\n")
				reader := bufio.NewReader(server)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					tag, command, _ := strings.Cut(strings.TrimSpace(line), " ")
					switch {
					case strings.HasPrefix(command, "EXAMINE "):
						fmt.Fprintf(server, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] epoch\r\n* OK [UIDNEXT 2] next\r\n%s OK selected\r\n", tag)
					case strings.Contains(command, "BODY.PEEK[]"):
						fmt.Fprint(server, tc.body)
						fmt.Fprintf(server, "%s OK fetched\r\n", tag)
					case strings.HasPrefix(command, "UID FETCH "):
						if strings.HasPrefix(tc.metadata, "NO ") {
							fmt.Fprintf(server, "%s %s", tag, tc.metadata)
						} else {
							fmt.Fprint(server, tc.metadata)
							fmt.Fprintf(server, "%s OK fetched\r\n", tag)
						}
					default:
						fmt.Fprintf(server, "%s BAD unexpected command\r\n", tag)
					}
				}
			}()
			client := &Client{conn: conn, client: imapclient.New(conn, nil), timeout: 2 * time.Second}
			defer func() { client.Close(); <-done }()
			body, err := client.Fetch(context.Background(), model.Ref{Folder: "Calendar", Validity: 1, UID: 1}, 1024)
			if tc.name == "metadata-failure" {
				if err == nil || errors.Is(err, model.ErrRemoteMessageGone) || errors.Is(err, model.ErrRemoteBodyUnavailable) {
					t.Fatal("metadata error misclassified", err)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), `"Calendar" UIDVALIDITY 1 UID 1`) {
				t.Fatal("missing fetch context", err)
			}
			if tc.name == "empty-body" && (body == nil || len(body) != 0) {
				t.Fatal("empty body mistaken for missing body")
			}
		})
	}
}
