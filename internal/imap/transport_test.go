package imap

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jp39/imaplocal/internal/auth"
	"github.com/jp39/imaplocal/internal/testutil"
)

func TestCONNECTVerifiedIMAP(t *testing.T) {
	c, _, _ := testutil.IMAP(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != c.Server {
			t.Error("incorrect CONNECT target")
			w.WriteHeader(400)
			return
		}
		upstream, err := net.Dial("tcp", c.Server)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		downstream, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer downstream.Close()
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		done := make(chan struct{})
		go func() { io.Copy(upstream, buffered); upstream.Close(); close(done) }()
		io.Copy(downstream, upstream)
		downstream.Close()
		<-done
	}))
	defer proxy.Close()
	c.ProxyURL = proxy.URL
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backend.Snapshot(ctx, "INBOX"); err != nil {
		t.Fatal(err)
	}
	backend.Close()
	c.CAFile = ""
	backend, err = Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
	if err == nil {
		backend.Close()
		t.Fatal("proxy bypassed certificate validation")
	}
}

func TestCONNECTDeniedAndCancellation(t *testing.T) {
	for _, stall := range []bool{false, true} {
		t.Run(map[bool]string{false: "denied", true: "cancelled"}[stall], func(t *testing.T) {
			c, _, _ := testutil.IMAP(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				if stall {
					<-release
				} else {
					w.WriteHeader(403)
				}
			}))
			defer proxy.Close()
			defer close(release)
			c.ProxyURL = proxy.URL
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				b, err := Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
				if b != nil {
					b.Close()
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("CONNECT did not arrive")
			}
			if stall {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected error")
				}
			case <-time.After(time.Second):
				t.Fatal("CONNECT did not stop")
			}
		})
	}
}
