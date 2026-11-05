package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const inboxCap = 200

// InboxEntry is one locally retained message record. It never leaves the machine.
type InboxEntry struct {
	At        int64  `json:"at"`
	MsgID     string `json:"msg_id"`
	Team      string `json:"team,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Body      string `json:"body"`
	Session   string `json:"session,omitempty"`
	Direction string `json:"direction"` // in | out
	Read      bool   `json:"read"`
}

type inbox struct {
	mu      sync.Mutex
	path    string
	entries []InboxEntry
}

func newInbox(stateDir string) *inbox {
	in := &inbox{path: filepath.Join(stateDir, "inbox.json")}
	if b, err := os.ReadFile(in.path); err == nil {
		json.Unmarshal(b, &in.entries)
	}
	return in
}

func (in *inbox) add(e InboxEntry) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.entries = append(in.entries, e)
	if len(in.entries) > inboxCap {
		in.entries = in.entries[len(in.entries)-inboxCap:]
	}
	in.saveLocked()
}

func (in *inbox) saveLocked() {
	b, _ := json.MarshalIndent(in.entries, "", "  ")
	os.MkdirAll(filepath.Dir(in.path), 0o700)
	os.WriteFile(in.path, b, 0o600)
}

// list returns the latest records for a session (empty = every session).
func (in *inbox) list(session string, limit int) []InboxEntry {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []InboxEntry
	for i := len(in.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := in.entries[i]
		if session != "" && e.Session != session {
			continue
		}
		out = append(out, e)
	}
	// oldest first for readability
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// drainUnread returns inbound unread entries for a session and marks them read.
func (in *inbox) drainUnread(session string) []InboxEntry {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []InboxEntry
	for i := range in.entries {
		e := &in.entries[i]
		if e.Session == session && e.Direction == "in" && !e.Read {
			e.Read = true
			out = append(out, *e)
		}
	}
	if len(out) > 0 {
		in.saveLocked()
	}
	return out
}
