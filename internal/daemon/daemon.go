package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jp39/imaplocal/internal/auth"
	"github.com/jp39/imaplocal/internal/config"
	remote "github.com/jp39/imaplocal/internal/imap"
	"github.com/jp39/imaplocal/internal/maildir"
	"github.com/jp39/imaplocal/internal/reconcile"
	"github.com/jp39/imaplocal/internal/status"
)

func Run(ctx context.Context, c config.Config, once bool, log *slog.Logger) error {
	engine, err := reconcile.Open(c)
	if err != nil {
		return err
	}
	defer engine.Close()
	engine.Log = log
	var lastProgress time.Time
	var lastPhase string
	engine.Progress = func(phase, folder string, completed, total int) {
		now := time.Now()
		if phase != lastPhase || completed == total || now.Sub(lastProgress) >= 5*time.Second {
			attrs := []any{"phase", phase}
			if folder != "" {
				attrs = append(attrs, "folder", folder)
			}
			attrs = append(attrs, "processed", completed, "total", total)
			log.Info("reconciliation progress", attrs...)
			lastProgress, lastPhase = now, phase
		}
	}
	health := status.Health{}
	if _, err = engine.State.Get("meta", "health", &health); err != nil {
		return err
	}
	defer func() {
		health.State = "stopped"
		health.Updated = time.Now().UTC()
		health.NextAttempt = time.Time{}
		_ = engine.State.Put("meta", "health", health)
	}()
	publishIdle := func(value status.IDLEState) {
		if err := engine.State.Put("idle", value.Folder, value); err != nil {
			log.Warn("could not save IDLE status", "folder", value.Folder, "error", err)
		}
	}
	previousIdle, err := engine.State.Keys("idle")
	if err != nil {
		return err
	}
	for _, folder := range previousIdle {
		publishIdle(status.IDLEState{Folder: folder, State: "stopped", Updated: time.Now().UTC()})
	}
	credentials := &auth.Command{Args: c.CredentialCommand}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	notifications := make(chan struct{}, 1)
	var workers sync.WaitGroup
	if !once {
		workers.Go(func() {
			if err := maildir.WatchEvents(ctx, engine.Local.Root, notifications, func(path, operation string) {
				log.Info("Maildir change detected", "filename", path, "operation", operation)
			}); err != nil && ctx.Err() == nil {
				log.Warn("filesystem notifications unavailable; periodic scans remain active", "error", err)
			}
		})
	}
	// Keep authenticated IDLE connections bounded on large accounts.
	watchers := map[string]context.CancelFunc{}
	refreshWatchers := func(folders []string) {
		if once || !c.Idle {
			return
		}
		desired := idleFolders(folders)
		for folder, stop := range watchers {
			if !desired[folder] {
				stop()
				delete(watchers, folder)
			}
		}
		for folder := range desired {
			if _, exists := watchers[folder]; exists {
				continue
			}
			watchCtx, stop := context.WithCancel(ctx)
			watchers[folder] = stop
			workers.Go(func() { watch(watchCtx, c, folder, credentials, notifications, log, publishIdle) })
		}
	}
	// Cancel workers before waiting, and wait before returning to the caller.
	defer func() { cancel(); workers.Wait() }()
	var client *remote.Client
	defer func() {
		if client != nil {
			client.Close()
		}
	}()
	backoff := time.Second
	for {
		if err = ctx.Err(); err != nil {
			return nil
		}
		health.LastAttempt = time.Now().UTC()
		health.Updated = health.LastAttempt
		health.NextAttempt = time.Time{}
		health.State = "syncing"
		if client == nil {
			health.State = "connecting"
		}
		if err = engine.State.Put("meta", "health", health); err != nil {
			return err
		}
		if client == nil {
			client, err = remote.Dial(ctx, c, credentials, nil)
		}
		if err == nil {
			if health.State != "syncing" {
				health.State = "syncing"
				health.Updated = time.Now().UTC()
				if err = engine.State.Put("meta", "health", health); err != nil {
					return err
				}
			}
			engine.Remote = client
			err = engine.Cycle(ctx)
			refreshWatchers(engine.SelectedFolders)
		}
		if ctx.Err() != nil {
			return nil
		}
		delay := c.PollInterval()
		if err != nil {
			health.State = "retrying"
			delay = backoff
			backoff = min(60*time.Second, backoff*2)
			health.Error = err.Error()
			if errors.Is(err, maildir.ErrChanged) {
				delay = time.Second
				backoff = time.Second
				log.Info("Maildir changed during reconciliation; rescanning", "error", err, "retry_in", delay)
			} else {
				log.Error("reconciliation failed; reconnecting", "error", err, "retry_in", delay)
			}
			if client != nil && !errors.Is(err, maildir.ErrChanged) {
				client.Close()
				client = nil
			}
		} else {
			health.State = "waiting"
			health.Error = ""
			health.LastSuccess = time.Now().UTC()
			backoff = time.Second
		}
		health.Updated = time.Now().UTC()
		health.NextAttempt = health.Updated.Add(delay)
		if once {
			health.State = "stopped"
			health.NextAttempt = time.Time{}
		}
		if saveErr := engine.State.Put("meta", "health", health); saveErr != nil {
			return saveErr
		}
		report, reportErr := status.Read(engine.State)
		if reportErr != nil {
			return reportErr
		}
		if err == nil && len(report.Issues) > 0 {
			log.Warn("reconciliation has unresolved issues", "entries", report.Entries, "pending_arrivals", report.PendingArrivals, "issues", len(report.Issues), "reason", strings.Join(status.Summaries(report.Issues), "; "))
		} else if err == nil {
			log.Info("reconciliation complete", "entries", report.Entries, "pending_arrivals", report.PendingArrivals, "issues", len(report.Issues))
		}
		if once {
			if err != nil {
				return err
			}
			if len(report.Issues) > 0 {
				return fmt.Errorf("reconciliation has %d unresolved issues; use status", len(report.Issues))
			}
			return nil
		}
		if !wait(ctx, delay, notifications) {
			return nil
		}
	}
}
func wait(ctx context.Context, d time.Duration, notify <-chan struct{}) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	case <-notify:
	}
	// Coalesce bursts, including the daemon's own observable writes.
	debounce := time.NewTimer(500 * time.Millisecond)
	defer debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-debounce.C:
			return true
		case <-notify:
		}
	}
}
func watch(ctx context.Context, c config.Config, folder string, credentials *auth.Command, notify chan<- struct{}, log *slog.Logger, update func(status.IDLEState)) {
	publish := func(state string, err error, retry time.Time) {
		value := status.IDLEState{Folder: folder, State: state, Updated: time.Now().UTC(), RetryAt: retry}
		if err != nil {
			value.Error = err.Error()
		}
		update(value)
	}
	events := make(chan struct{}, 1)
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		for {
			select {
			case <-relayCtx.Done():
				return
			case <-events:
				log.Info("IMAP mailbox change detected", "folder", folder)
				select {
				case notify <- struct{}{}:
				default:
				}
			}
		}
	}()
	defer func() { stopRelay(); <-relayDone }()
	terminal := "stopped"
	defer func() { publish(terminal, nil, time.Time{}) }()
	backoff := time.Second
	for ctx.Err() == nil {
		publish("connecting", nil, time.Time{})
		client, err := remote.Dial(ctx, c, credentials, events)
		if err == nil {
			publish("connected", nil, time.Time{})
			log.Info("IDLE connected", "folder", folder)
			select {
			case notify <- struct{}{}:
			default:
			}
			started := time.Now()
			err = client.Watch(ctx, folder)
			client.Close()
			if time.Since(started) > time.Minute {
				backoff = time.Second
			}
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, remote.ErrNoIdle) {
			terminal = "unsupported"
			log.Warn("IDLE unavailable; using periodic inventory", "folder", folder)
			return
		}
		publish("retrying", err, time.Now().UTC().Add(backoff))
		log.Warn("IDLE disconnected; reconnecting", "folder", folder, "error", err, "retry_in", backoff)
		select {
		case notify <- struct{}{}:
		default:
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// Prefer INBOX; excess folders are still reconciled on the periodic timer.
func idleFolders(folders []string) map[string]bool {
	const limit = 8
	ordered := append([]string(nil), folders...)
	sort.Strings(ordered)
	for i, f := range ordered {
		if strings.EqualFold(f, "INBOX") {
			ordered[0], ordered[i] = ordered[i], ordered[0]
			break
		}
	}
	selected := map[string]bool{}
	for _, f := range ordered {
		if len(selected) >= limit {
			break
		}
		selected[f] = true
	}
	return selected
}
