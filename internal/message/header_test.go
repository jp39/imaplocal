package message

import (
	"bytes"
	"reflect"
	"testing"
)

func TestHeadersRecoverAfterMalformedFieldsWithoutChangingRaw(t *testing.T) {
	for _, newline := range []string{"\r\n", "\n"} {
		t.Run(map[string]string{"\r\n": "CRLF", "\n": "LF"}[newline], func(t *testing.T) {
			raw := bytes.Join([][]byte{
				[]byte(" orphan continuation"),
				[]byte("From: sender@example.com"),
				[]byte("${Subject}: invalid"),
				[]byte("\tTo: fake@example.com"),
				[]byte("no colon here"),
				[]byte("Subject: valid folded"),
				[]byte("\tsubject"),
				[]byte("To: real@example.com"),
				[]byte("Message-ID: <recover@example.com>"),
				[]byte("X-Test: first"), []byte("X-Test: second"),
				{}, []byte("To: body@example.com"),
			}, []byte(newline))
			before := append([]byte(nil), raw...)
			header, ignored := Headers(raw)
			if ignored != 3 || header.Get("From") != "sender@example.com" || header.Get("To") != "real@example.com" || header.Get("Subject") != "valid folded subject" || header.Get("Message-ID") != "<recover@example.com>" || !reflect.DeepEqual(header.Values("X-Test"), []string{"first", "second"}) {
				t.Fatalf("lost or invented headers: %+v ignored=%d", header, ignored)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("raw bytes modified")
			}
		})
	}
}

func TestHeadersFinalLineAndEmptyInput(t *testing.T) {
	h, n := Headers([]byte("Broken\nTo: final@example.com"))
	if n != 1 || h.Get("To") != "final@example.com" {
		t.Fatalf("final line lost: %+v %d", h, n)
	}
	h, n = Headers(nil)
	if n != 0 || len(h) != 0 {
		t.Fatal(h, n)
	}
}
