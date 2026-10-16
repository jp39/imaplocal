package reconcile

import "github.com/jp39/imaplocal/internal/model"

// MergeFlags propagates independent bit changes relative to the last agreed
// state. A boolean bit changed on both sides necessarily has the same value.
func MergeFlags(base, local, remote model.Flags) model.Flags {
	changed := local ^ base
	return ((remote &^ changed) | (local & changed)) & model.AllFlags
}
