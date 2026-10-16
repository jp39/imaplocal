package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	protocol "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/jp39/imaplocal/internal/config"
)

type observedSession struct {
	imapserver.Session
	ready chan<- struct{}
}

func (s *observedSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	select {
	case s.ready <- struct{}{}:
	default:
	}
	return s.Session.Idle(w, stop)
}
func (s *observedSession) Move(w *imapserver.MoveWriter, set protocol.NumSet, dest string) error {
	return s.Session.(imapserver.SessionMove).Move(w, set, dest)
}

func IMAP(t *testing.T) (config.Config, *imapmemserver.User, <-chan struct{}) {
	t.Helper()
	return IMAPWithCaps(t, protocol.CapSet{protocol.CapIMAP4rev1: {}, protocol.CapUIDPlus: {}, protocol.CapMove: {}, protocol.CapIdle: {}})
}

func IMAPWithCaps(t *testing.T, caps protocol.CapSet) (config.Config, *imapmemserver.User, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "imaplocal test server"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	user := imapmemserver.NewUser("test", "secret")
	if err = user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 10)
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &observedSession{Session: mem.NewSession(), ready: ready}, nil, nil
	}, Caps: caps, Logger: log.New(io.Discard, "", 0)})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(tlsListener) }()
	t.Cleanup(func() { srv.Close(); tlsListener.Close(); <-done })
	c := config.Config{Server: listener.Addr().String(), Username: "test", Auth: "plain", CredentialCommand: []string{"/usr/bin/printf", "secret"}, CAFile: ca, Maildir: filepath.Join(dir, "mail"), Folders: []string{"INBOX"}, RemoteWrites: true, Timeout: "5s"}
	if err = c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c, user, ready
}
