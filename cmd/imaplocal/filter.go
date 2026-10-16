package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jp39/imaplocal/internal/auth"
	"github.com/jp39/imaplocal/internal/config"
	remote "github.com/jp39/imaplocal/internal/imap"
	"github.com/jp39/imaplocal/internal/reconcile"
)

type stringFlags []string

func (s *stringFlags) String() string     { return strings.Join(*s, ", ") }
func (s *stringFlags) Set(v string) error { *s = append(*s, v); return nil }

func filterCommand(ctx context.Context, c config.Config, opts reconcile.FilterOptions, apply bool, out io.Writer) error {
	var engine *reconcile.Engine
	var err error
	if apply {
		if !c.RemoteWrites {
			return errors.New("filter -apply requires remote_writes=true")
		}
		engine, err = reconcile.Open(c)
	} else {
		engine, err = reconcile.View(c)
	}
	if err != nil {
		return err
	}
	defer engine.Close()
	if apply {
		engine.Log = slog.Default()
	}
	if !opts.Since.IsZero() {
		fmt.Fprintf(out, "Received on/after: %s (IMAP INTERNALDATE)\n", receivedTime(opts.Since))
	}
	preview, err := engine.PlanFilters(ctx, opts)
	if err != nil {
		return err
	}
	for _, skipped := range preview.Skipped {
		fmt.Fprintf(out, "Skipped: %q UIDVALIDITY %d UID %d received=%s Message-ID=%q filename=%q: %s\n", skipped.Ref.Folder, skipped.Ref.Validity, skipped.Ref.UID, receivedTime(skipped.ReceivedAt), skipped.MessageID, skipped.Filename, skipped.Reason)
	}
	for _, plan := range preview.Plans {
		targets := make([]string, 0, len(plan.Targets))
		for _, target := range plan.Targets {
			targets = append(targets, fmt.Sprintf("%q flags=%q", target.Folder, target.Flags.Maildir()))
		}
		fmt.Fprintf(out, "%q UID %d received=%s <%s> %q -> %s\n", plan.Entry.Remote.Folder, plan.Entry.Remote.UID, receivedTime(plan.ReceivedAt), plan.MessageID, plan.Subject, strings.Join(targets, ", "))
	}
	if !apply {
		fmt.Fprintf(out, "Dry run: %d matched, %d would change, %d skipped. No connection or mail/state changes made.\n", preview.Matched, len(preview.Plans), len(preview.Skipped))
		return nil
	}
	if len(preview.Plans) > 0 {
		if err = engine.CheckFilterReady(); err != nil {
			return err
		}
		backend, err := remote.Dial(ctx, c, &auth.Command{Args: c.CredentialCommand}, nil)
		if err != nil {
			return err
		}
		defer backend.Close()
		engine.Remote = backend
		if err = engine.ApplyFilters(ctx, preview.Plans); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Applied: %d matched, %d filtered, %d skipped.\n", preview.Matched, len(preview.Plans), len(preview.Skipped))
	return nil
}

// Resolve relative dates once per invocation, so every message uses one cutoff.
func filterCutoff(since, within string, now time.Time) (time.Time, error) {
	if since != "" && within != "" {
		return time.Time{}, errors.New("-since and -within are mutually exclusive")
	}
	if since != "" {
		if date, err := time.ParseInLocation("2006-01-02", since, time.Local); err == nil {
			return date, nil
		}
		if date, err := time.Parse(time.RFC3339Nano, since); err == nil {
			return date, nil
		}
		return time.Time{}, errors.New("-since requires YYYY-MM-DD or an RFC3339 timestamp with timezone")
	}
	if within != "" {
		durationText := within
		if strings.HasSuffix(within, "d") {
			days, err := strconv.ParseInt(strings.TrimSuffix(within, "d"), 10, 64)
			if err != nil || days <= 0 || days > ((1<<63-1)/int64(24*time.Hour)) {
				return time.Time{}, errors.New("-within requires a positive whole number of days, e.g. 10d")
			}
			durationText = strconv.FormatInt(days*24, 10) + "h"
		}
		duration, err := time.ParseDuration(durationText)
		if err != nil || duration <= 0 {
			return time.Time{}, errors.New("-within requires a positive lookback, e.g. 10d or 24h")
		}
		return now.Add(-duration), nil
	}
	return time.Time{}, nil
}

func receivedTime(date time.Time) string {
	if date.IsZero() {
		return "unknown"
	}
	return date.In(time.Local).Format(time.RFC3339Nano)
}
