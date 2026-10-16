package maildir

import (
	"context"
	"io/fs"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

func Watch(ctx context.Context, root string, notify chan<- struct{}) error {
	return WatchEvents(ctx, root, notify, nil)
}

func WatchEvents(ctx context.Context, root string, notify chan<- struct{}, eventLog func(path, operation string)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	add := func(start string) error {
		return filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if privateEvent(root, path) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return watcher.Add(path)
			}
			return nil
		})
	}
	if err = add(root); err != nil {
		return err
	}
	signal := func() {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if privateEvent(root, event.Name) {
				continue
			}
			if eventLog != nil {
				eventLog(event.Name, event.Op.String())
			}
			signal()
			if event.Op&fsnotify.Create != 0 {
				_ = add(event.Name)
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			signal() // periodic full scans recover lost notifications
		}
	}
}
