package auth

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Command struct {
	Args []string
	mu   sync.Mutex
}
type bounded struct{ bytes.Buffer }

func (w *bounded) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 16384 {
		return 0, errors.New("credential output exceeds limit")
	}
	return w.Buffer.Write(p)
}
func (p *Command) Credential(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Args) == 0 {
		return "", errors.New("credential command is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Args[0], p.Args[1:]...)
	var out bounded
	cmd.Stdout = &out
	// Never include command stderr or a bearer token in an error.
	if err := cmd.Run(); err != nil {
		return "", errors.New("credential command failed (check authorization and credential storage)")
	}
	token := strings.TrimSuffix(strings.TrimSuffix(out.String(), "\n"), "\r")
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return "", errors.New("credential command must return exactly one nonempty line")
	}
	return token, nil
}
