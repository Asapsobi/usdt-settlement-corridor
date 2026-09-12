// Package auditlog is the ops console's own append-only write record --
// invariant 3: every write action is logged, server-side, BEFORE the
// downstream call is made, independent of whatever the downstream
// service's own audit trail records (see docs/03-build/
// ops-console-build-prompts.md's OC.8). A plain JSON-lines file, not a
// database -- this project has no shared logging infrastructure to plug
// into yet, and a file needs no migration, no connection, no schema.
package auditlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry is one audit-log line.
type Entry struct {
	Time     time.Time      `json:"time"`
	Operator string         `json:"operator"`
	Action   string         `json:"action"`
	Target   string         `json:"target"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// Log appends entries to one file, one JSON object per line.
type Log struct {
	mu   sync.Mutex
	file *os.File
}

// Open opens (creating if necessary) the audit log at path, appending to
// whatever is already there.
func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("auditlog: opening %s: %w", path, err)
	}
	return &Log{file: f}, nil
}

// Write appends one entry -- called by every OC.3-OC.7 write handler
// BEFORE the downstream call, per invariant 3, so an attempt is on
// record even if the downstream call itself then fails.
func (l *Log) Write(operator, action, target string, detail map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry := Entry{Time: time.Now().UTC(), Operator: operator, Action: action, Target: target, Detail: detail}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("auditlog: encoding entry: %w", err)
	}
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("auditlog: writing entry: %w", err)
	}
	return nil
}

// Close closes the underlying file.
func (l *Log) Close() error {
	return l.file.Close()
}

// Tail reads the last n entries from the audit log at path, oldest
// first -- OC.8's own GET /audit read view. Bounded, not a queryable
// store: this is an operator-convenience page, not a log database.
func Tail(path string, n int) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("auditlog: opening %s: %w", path, err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("auditlog: reading %s: %w", path, err)
	}

	entries := make([]Entry, 0, len(lines))
	for _, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // a malformed line is skipped, not fatal to reading the rest
		}
		entries = append(entries, e)
	}
	return entries, nil
}
