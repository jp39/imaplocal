//go:build darwin && cgo

package maildir

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsevents"
)

func Watch(ctx context.Context, root string, notify chan<- struct{}) error {
	return WatchEvents(ctx, root, notify, nil)
}

func WatchEvents(ctx context.Context, root string, notify chan<- struct{}, eventLog func(path, operation string)) error {
	stream := &fsevents.EventStream{Paths: []string{root}, Flags: fsevents.FileEvents | fsevents.NoDefer | fsevents.WatchRoot, Latency: 250 * time.Millisecond, Events: make(chan []fsevents.Event, 64)}
	if err := stream.Start(); err != nil {
		return err
	}
	defer stream.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case events, ok := <-stream.Events:
			if !ok {
				return errors.New("FSEvents stream stopped")
			}
			changed := false
			for _, event := range events {
				if event.Flags&fsevents.RootChanged != 0 {
					return errors.New("Maildir watch root changed")
				}
				if !privateEvent(root, event.Path) {
					changed = true
					if eventLog != nil {
						path := event.Path
						if !filepath.IsAbs(path) {
							path = "/" + path
						}
						var ops []string
						for _, op := range []struct {
							flag fsevents.EventFlags
							name string
						}{
							{fsevents.ItemCreated, "create"}, {fsevents.ItemRemoved, "remove"},
							{fsevents.ItemRenamed, "rename"}, {fsevents.ItemModified, "write"},
							{fsevents.MustScanSubDirs, "rescan"},
						} {
							if event.Flags&op.flag != 0 {
								ops = append(ops, op.name)
							}
						}
						if len(ops) == 0 {
							ops = append(ops, "metadata")
						}
						eventLog(path, strings.Join(ops, "|"))
					}
				}
			}
			if changed {
				select {
				case notify <- struct{}{}:
				default:
				}
			}
		}
	}
}
