package status

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jp39/imaplocal/internal/model"
	"github.com/jp39/imaplocal/internal/state"
)

type Health struct {
	State       string
	Updated     time.Time
	NextAttempt time.Time
	LastAttempt time.Time
	LastSuccess time.Time
	Error       string
}
type Report struct {
	Folders          []FolderProgress
	Health           Health
	Entries          int
	Tombstones       int
	PendingArrivals  int
	FolderOperations []model.FolderOperation
	Operations       []model.Operation
	Issues           []model.Issue
}

func Read(s *state.Store) (Report, error) {
	var r Report
	if _, err := s.Get("meta", "health", &r.Health); err != nil {
		return r, err
	}

	summary, err := s.Summary()
	if err != nil {
		return r, err
	}
	r.Entries = summary.Entries
	r.Tombstones = summary.Tombstones
	r.PendingArrivals = summary.PendingArrivals
	r.Operations = summary.Operations
	r.FolderOperations = summary.FolderOperations
	r.Issues, err = state.List[model.Issue](s, "issue")
	if err != nil {
		return r, err
	}
	progress, err := state.List[FolderProgress](s, "folder_progress")
	if err != nil {
		return r, err
	}
	records, err := state.List[model.Folder](s, "folder")
	if err != nil {
		return r, err
	}
	idle, err := state.List[IDLEState](s, "idle")
	if err != nil {
		return r, err
	}
	retired := map[string]bool{}
	folders := map[string]FolderProgress{}
	for _, record := range records {
		retired[record.Name] = record.Retired
		if !record.Retired {
			folders[record.Name] = FolderProgress{Name: record.Name}
		}
	}
	for _, p := range progress {
		p.Mapped = 0
		p.IDLE = IDLEState{}
		if !retired[p.Name] {
			folders[p.Name] = p
		}
	}
	for name, count := range summary.Mapped {
		if !retired[name] {
			p := folders[name]
			p.Name = name
			p.Mapped = count
			folders[name] = p
		}
	}
	for _, i := range idle {
		if p, ok := folders[i.Folder]; ok {
			p.IDLE = i
			folders[i.Folder] = p
		}
	}
	names := map[string]bool{}
	for name := range folders {
		names[name] = true
	}
	for _, name := range model.SortedFolders(names) {
		r.Folders = append(r.Folders, folders[name])
	}
	return r, nil
}

// FolderProgress is the latest successful inventory, plus the current attempt.
// These are observations, not a second synchronization ledger.
type FolderProgress struct {
	Name           string
	RemoteMessages int
	Mapped         int
	LastInventory  time.Time
	State          string
	Error          string
	IDLE           IDLEState
}

type IDLEState struct {
	Folder  string
	State   string
	Updated time.Time
	RetryAt time.Time
	Error   string
}

// Summaries is shared by status and warning logs, so a count explains itself.
func Summaries(issues []model.Issue) []string {
	counts := map[string]int{}
	for _, issue := range issues {
		counts[issue.Kind+"\x00"+issue.Detail]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []string{}
	for _, key := range keys {
		kind, detail, _ := strings.Cut(key, "\x00")
		count := counts[key]
		noun := "issue"
		if count != 1 {
			noun = "issues"
		}
		if kind == "writes_disabled" {
			if strings.HasPrefix(detail, "local flags differ") {
				noun = "local flag change"
				if count != 1 {
					noun += "s"
				}
				out = append(out, fmt.Sprintf("%d %s blocked: remote writes disabled", count, noun))
				continue
			}
			if strings.HasPrefix(detail, "unmapped local message") {
				noun = "local upload"
				if count != 1 {
					noun += "s"
				}
				out = append(out, fmt.Sprintf("%d %s blocked: remote writes disabled", count, noun))
				continue
			}
		}
		out = append(out, fmt.Sprintf("%d %s (%s): %s", count, noun, kind, detail))
	}
	return out
}

func timestamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

func Text(w io.Writer, r Report, remoteWrites bool) error {
	var b strings.Builder
	mode := "read-only"
	if remoteWrites {
		mode = "bidirectional"
	}
	fmt.Fprintf(&b, "Mode: %s\n", mode)
	last := r.Health.State
	if last == "" {
		last = "unknown (restart imaplocal to publish current state)"
	}
	fmt.Fprintf(&b, "Last state: %s", last)
	if !r.Health.Updated.IsZero() {
		fmt.Fprintf(&b, " (as of %s)", timestamp(r.Health.Updated))
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Last completed cycle: %s\n", timestamp(r.Health.LastSuccess))
	if !r.Health.NextAttempt.IsZero() {
		fmt.Fprintf(&b, "Next scheduled attempt: %s (notifications may trigger earlier)\n", timestamp(r.Health.NextAttempt))
	}
	if r.Health.Error != "" {
		fmt.Fprintf(&b, "Last error: %s\n", r.Health.Error)
	}
	fmt.Fprintf(&b, "Messages: %d tracked, %d retired\nPending: %d arrivals, %d message operations, %d folder operations\n", r.Entries-r.Tombstones, r.Tombstones, r.PendingArrivals, len(r.Operations), len(r.FolderOperations))
	if len(r.Issues) == 0 {
		fmt.Fprintln(&b, "Issues: none")
	} else {
		fmt.Fprintln(&b, "Issues:")
		for _, line := range Summaries(r.Issues) {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	if len(r.Folders) > 0 {
		fmt.Fprintln(&b, "\nFolders (last known observations):")
		table := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "Folder\tRemote\tMapped\tLast inventory\tConnection")
		for _, folder := range r.Folders {
			connection := "periodic"
			if r.Health.Updated.IsZero() {
				connection = "unknown"
			}
			if folder.IDLE.State != "" {
				connection = "IDLE " + folder.IDLE.State
			}
			if !folder.IDLE.RetryAt.IsZero() {
				connection += "; retry " + timestamp(folder.IDLE.RetryAt)
			}
			remote := "—"
			if !folder.LastInventory.IsZero() {
				remote = strconv.Itoa(folder.RemoteMessages)
			}
			inventory := timestamp(folder.LastInventory)
			if folder.LastInventory.IsZero() {
				inventory = "not recorded"
			}
			fmt.Fprintf(table, "%s\t%s\t%d\t%s\t%s\n", strconv.Quote(folder.Name), remote, folder.Mapped, inventory, connection)
			if folder.Error != "" {
				fmt.Fprintf(table, "  Inventory error: %s\n", folder.Error)
			}
			if folder.State == "inventory" {
				fmt.Fprintln(table, "  Inventory in progress")
			}
			if folder.IDLE.Error != "" {
				fmt.Fprintf(table, "  IDLE: %s\n", folder.IDLE.Error)
			}
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	for _, op := range r.Operations {
		if op.Kind == "delete" {
			fmt.Fprintf(&b, "\nMessage operation %s: delete, %s UID %d (UIDVALIDITY %d), %s\n", op.ID, strconv.Quote(op.Source.Folder), op.Source.UID, op.Source.Validity, op.Phase)
		} else {
			fmt.Fprintf(&b, "\nMessage operation %s: %s, %s -> %s, %s\n", op.ID, op.Kind, strconv.Quote(op.Source.Folder), strconv.Quote(op.Destination), op.Phase)
		}
		if op.Error != "" {
			fmt.Fprintf(&b, "  %s\n", op.Error)
		}
	}
	for _, op := range r.FolderOperations {
		fmt.Fprintf(&b, "\nFolder operation %s: %s, %s -> %s, %s\n", op.ID, op.Kind, strconv.Quote(op.Source), strconv.Quote(op.Destination), op.Phase)
		if op.Error != "" {
			fmt.Fprintf(&b, "  %s\n", op.Error)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
