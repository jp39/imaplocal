//go:build (!darwin && !linux) || (darwin && !cgo)

package maildir

import (
	"context"
	"errors"
)

func Watch(ctx context.Context, root string, notify chan<- struct{}) error {
	return WatchEvents(ctx, root, notify, nil)
}
func WatchEvents(context.Context, string, chan<- struct{}, func(string, string)) error {
	return errors.New("native filesystem watcher unavailable in this build")
}
