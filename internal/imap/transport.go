package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/jp39/imaplocal/internal/config"
)

// bufferedConn retains any bytes read ahead while parsing CONNECT headers.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func dialTLS(ctx context.Context, c config.Config, targetTLS *tls.Config) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.CommandTimeout())
	defer cancel()
	address := c.Server
	var proxy *url.URL
	if c.ProxyURL != "" {
		var err error
		proxy, err = url.Parse(c.ProxyURL)
		if err != nil || proxy.Hostname() == "" || (proxy.Scheme != "http" && proxy.Scheme != "https") || proxy.User != nil {
			return nil, errors.New("invalid proxy_url")
		}
		port := proxy.Port()
		if port == "" {
			if proxy.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		address = net.JoinHostPort(proxy.Hostname(), port)
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			raw.Close()
		}
	}()
	// Cancellation must interrupt blocked CONNECT reads as well as TLS handshakes.
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { raw.Close(); close(stopped) })
	disarmed := false
	defer func() {
		if !disarmed && !stop() {
			<-stopped
		}
	}()
	deadline, _ := ctx.Deadline()
	raw.SetDeadline(deadline)
	var conn net.Conn = raw
	if proxy != nil {
		if proxy.Scheme == "https" {
			// Proxy trust uses the system roots, independent of the IMAP CA override.
			secure := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: proxy.Hostname()})
			if err = secure.HandshakeContext(ctx); err != nil {
				return nil, fmt.Errorf("proxy TLS handshake: %w", err)
			}
			conn = secure
		}
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: c.Server}, Host: c.Server, Header: make(http.Header)}
		if err = req.Write(conn); err != nil {
			return nil, fmt.Errorf("proxy CONNECT request: %w", err)
		}
		reader := bufio.NewReader(io.LimitReader(conn, 64<<10))
		response, e := http.ReadResponse(reader, req)
		if e != nil {
			return nil, errors.New("proxy CONNECT response could not be read")
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("proxy CONNECT rejected (HTTP %d)", response.StatusCode)
		}
		// Transfer buffered tunnel bytes without keeping the header size limit on data.
		buffered := reader.Buffered()
		prefix := make([]byte, buffered)
		io.ReadFull(reader, prefix)
		conn = &bufferedConn{Conn: conn, reader: bufio.NewReader(io.MultiReader(bytes.NewReader(prefix), conn))}
	}
	secure := tls.Client(conn, targetTLS)
	if err = secure.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("IMAP TLS handshake: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Disarm cancellation before returning ownership to the IMAP client.
	disarmed = true
	if !stop() {
		<-stopped
		return nil, ctx.Err()
	}
	raw.SetDeadline(time.Time{})
	success = true
	return secure, nil
}
