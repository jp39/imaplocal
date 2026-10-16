package reconcile

import (
	"github.com/jp39/imaplocal/internal/message"

	"github.com/jp39/imaplocal/internal/model"
)

// Message-ID is diagnostic context, never a synchronization identity. Parse only
// headers, leaving the stored message and its signatures byte-for-byte intact.
func (e *Engine) messageEvent(event string, ref model.Ref, raw []byte, attrs ...any) {
	if e.Log == nil {
		return
	}
	header, ignored := message.Headers(raw)
	if ignored > 0 {
		attrs = append(attrs, "ignored_headers", ignored)
	}
	fields := []any{"folder", ref.Folder, "uidvalidity", ref.Validity, "uid", ref.UID, "message_id", header.Get("Message-ID")}
	e.Log.Info(event, append(fields, attrs...)...)
}
