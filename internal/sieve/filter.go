package sieve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	gosieve "github.com/foxcpp/go-sieve"
	"github.com/foxcpp/go-sieve/interp"
	"github.com/jp39/imaplocal/internal/config"
	"github.com/jp39/imaplocal/internal/message"
	"github.com/jp39/imaplocal/internal/model"
)

type Filter struct {
	script *gosieve.Script
	Hash   string
}
type Target struct {
	Folder string
	Flags  model.Flags
}

func Load(path string) (*Filter, error) {
	f := &Filter{}
	if path == "" {
		return f, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f.Hash = fmt.Sprintf("%x", sha256.Sum256(raw))
	f.script, err = gosieve.Load(bytes.NewReader(raw), gosieve.DefaultOptions())
	if err != nil {
		return nil, err
	}
	for _, extension := range f.script.Extensions() {
		switch strings.ToLower(extension) {
		case "fileinto", "copy", "imap4flags":
		default:
			return nil, fmt.Errorf("unsupported Sieve extension %q", extension)
		}
	}
	return f, nil
}
func flags(f model.Flags) []string {
	out := []string{}
	for _, p := range []struct {
		f model.Flags
		s string
	}{{model.Seen, `\Seen`}, {model.Answered, `\Answered`}, {model.Flagged, `\Flagged`}, {model.Draft, `\Draft`}, {model.Forwarded, "$Forwarded"}} {
		if f&p.f != 0 {
			out = append(out, p.s)
		}
	}
	return out
}
func parse(fs []string) (model.Flags, error) {
	var out model.Flags
	for _, s := range fs {
		switch strings.ToLower(s) {
		case `\seen`:
			out |= model.Seen
		case `\answered`:
			out |= model.Answered
		case `\flagged`:
			out |= model.Flagged
		case `\draft`:
			out |= model.Draft
		case "$forwarded":
			out |= model.Forwarded
		default:
			return 0, fmt.Errorf("Sieve flag %q is not representable", s)
		}
	}
	return out, nil
}
func (f *Filter) Evaluate(ctx context.Context, source string, raw []byte, initial model.Flags) ([]Target, error) {
	if f.script == nil {
		return []Target{{source, initial}}, nil
	}
	header, _ := message.Headers(raw)
	var err error
	// No SMTP envelope is available from IMAP. Do not invent one from From/To.
	data := gosieve.NewRuntimeData(f.script, interp.DummyPolicy{}, interp.EnvelopeStatic{}, interp.MessageStatic{Size: len(raw), Header: header, RawMessage: raw})
	data.Flags = flags(initial)
	data.Env = interp.MapEnv{"name": "imaplocal", "mailbox": source}
	if err = f.script.Execute(ctx, data); err != nil {
		return nil, err
	}
	out := []Target{}
	seen := map[string]model.Flags{}
	for _, action := range data.AppliedActions {
		var target Target
		switch a := action.(type) {
		case interp.ActionKeep:
			target.Folder = source
			target.Flags, err = parse(a.Flags)
		case interp.ActionFileInto:
			target.Folder = a.Mailbox
			target.Flags, err = parse(a.Flags)
		default:
			return nil, fmt.Errorf("unsupported Sieve action %T; original retained", action)
		}
		if err != nil {
			return nil, err
		}
		if err = config.Folder(target.Folder); err != nil {
			return nil, err
		}
		if prior, ok := seen[target.Folder]; ok {
			if prior != target.Flags {
				return nil, fmt.Errorf("conflicting Sieve flags for %s", target.Folder)
			}
			continue
		}
		seen[target.Folder] = target.Flags
		out = append(out, target)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Sieve produced no delivery; original retained")
	}
	return out, nil
}
