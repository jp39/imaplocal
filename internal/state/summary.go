package state

import (
	"encoding/json"

	"github.com/jp39/imaplocal/internal/model"
)

// These expressions also occur in the indexes. Missing fields retain the zero
// values used when decoding older records into Go structs.
const (
	entryTombstone         = `coalesce(json_extract(data, '$.Tombstone'), 0)`
	entryFolder            = `coalesce(json_extract(data, '$.Remote.Folder'), '')`
	pendingArrival         = `kind='arrival' AND coalesce(json_extract(data, '$.Done'), 0)=0`
	pendingOperation       = `kind='operation' AND (coalesce(json_extract(data, '$.Phase'), '')!='done' OR (coalesce(json_extract(data, '$.ArrivalID'), '')='' AND coalesce(json_extract(data, '$.Applied'), 0)=0))`
	pendingFolderOperation = `kind='folder_operation' AND coalesce(json_extract(data, '$.Phase'), '')!='done'`
)

// Optional secondary indexes accelerate read-only status without a second
// ledger or cached counters. SQLite updates them atomically with each record.
const summaryIndexes = `
CREATE INDEX IF NOT EXISTS entry_summary ON records (` + entryTombstone + `, ` + entryFolder + `) WHERE kind='entry';
CREATE INDEX IF NOT EXISTS pending_arrival ON records(id) WHERE ` + pendingArrival + `;
CREATE INDEX IF NOT EXISTS pending_operation ON records(id) WHERE ` + pendingOperation + `;
CREATE INDEX IF NOT EXISTS pending_folder_operation ON records(id) WHERE ` + pendingFolderOperation + `;`

type SyncSummary struct {
	Entries, Tombstones, PendingArrivals int
	Mapped                               map[string]int
	Operations                           []model.Operation
	FolderOperations                     []model.FolderOperation
}

func (s *Store) Summary() (SyncSummary, error) {
	out := SyncSummary{Mapped: map[string]int{}}
	var indexCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('entry_summary','pending_arrival','pending_operation','pending_folder_operation')`).Scan(&indexCount); err != nil {
		return out, err
	}
	indexed := indexCount == 4
	rows, err := s.db.Query(`SELECT ` + entryTombstone + `, ` + entryFolder + `, count(*) FROM ` + summaryTable("entry_summary", indexed) + ` WHERE kind='entry' GROUP BY ` + entryTombstone + `, ` + entryFolder)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var retired bool
		var folder string
		var count int
		if err = rows.Scan(&retired, &folder, &count); err != nil {
			rows.Close()
			return out, err
		}
		out.Entries += count
		if retired {
			out.Tombstones += count
		} else {
			out.Mapped[folder] += count
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM ` + summaryTable("pending_arrival", indexed) + ` WHERE ` + pendingArrival).Scan(&out.PendingArrivals); err != nil {
		return out, err
	}
	out.Operations, err = listPending[model.Operation](s, pendingOperation, "pending_operation", indexed)
	if err != nil {
		return out, err
	}
	out.FolderOperations, err = listPending[model.FolderOperation](s, pendingFolderOperation, "pending_folder_operation", indexed)
	return out, err
}

// Predicates are internal constants, never user-supplied SQL.
func listPending[T any](s *Store, predicate, index string, indexed bool) ([]T, error) {
	rows, err := s.db.Query(`SELECT data FROM ` + summaryTable(index, indexed) + ` WHERE ` + predicate + ` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var record T
		if err = json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// With no ANALYZE statistics, SQLite may prefer the generic kind index and
// reparse all JSON. Select our purpose-built indexes explicitly when present;
// old read-only databases still work without requiring a schema write.
func summaryTable(index string, indexed bool) string {
	if indexed {
		return "records INDEXED BY " + index
	}
	return "records"
}
