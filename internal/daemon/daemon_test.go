package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dengquanjie/teamconnect/internal/proto"
)

type fakePeer struct {
	path  string
	lines chan []string
}

func startFakePeer(t *testing.T, dir, response string) *fakePeer {
	t.Helper()
	path := filepath.Join(dir, "peer.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakePeer{path: path, lines: make(chan []string, 1)}
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		sc := bufio.NewScanner(c)
		sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
		var got []string
		for len(got) < 2 && sc.Scan() {
			got = append(got, sc.Text())
		}
		fp.lines <- got
		if response != "" {
			c.Write([]byte(response + "\n"))
		}
	}()
	t.Cleanup(func() { l.Close() })
	return fp
}

func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	dir := t.TempDir()
	d := New(Config{
		SessionsDir: filepath.Join(dir, "sessions"),
		StateDir:    filepath.Join(dir, "state"),
	})
	os.MkdirAll(d.cfg.SessionsDir, 0o700)
	d.inboundPolicy = func() string { return "" }
	return d
}

func writeKey(t *testing.T, sessionsDir string, pid int, token string) {
	t.Helper()
	name := filepath.Join(sessionsDir, fmt.Sprintf("%d.abc.key", pid))
	if err := os.WriteFile(name, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInjectWritesAuthAndUserFrames(t *testing.T) {
	d := testDaemon(t)
	fp := startFakePeer(t, t.TempDir(), "")
	writeKey(t, d.cfg.SessionsDir, 1234, "peer-token-xyz")

	f := proto.Frame{
		Type: "deliver", MsgID: "m1", Team: "proj", Body: "hello <world> & friends",
		From: &proto.Peer{User: "alice", Session: "a1", Machine: "ma"},
	}
	status, reason := d.inject(registryEntry{Pid: 1234, SessionID: "s1", MessagingSocketPath: fp.path}, f)
	if status != proto.StatusDelivered {
		t.Fatalf("expected delivered, got %s (%s)", status, reason)
	}

	lines := <-fp.lines
	if len(lines) != 2 {
		t.Fatalf("expected 2 frames, got %d: %v", len(lines), lines)
	}
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &auth); err != nil {
		t.Fatalf("auth frame not JSON: %v", err)
	}
	if auth.Type != "auth" || auth.Token != "peer-token-xyz" {
		t.Fatalf("bad auth frame: %+v", auth)
	}

	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &msg); err != nil {
		t.Fatalf("user frame not JSON: %v", err)
	}
	if msg.Type != "user" || msg.Message.Role != "user" {
		t.Fatalf("bad user frame: %+v", msg)
	}
	content := msg.Message.Content
	for _, want := range []string{
		`from="alice/a1"`, `team="proj"`, `trust="cross-user"`, `msg_id="m1"`,
		"hello <world> & friends", "</teamconnect-message>",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("envelope missing %q:\n%s", want, content)
		}
	}
	tc, _ := msg.Meta["teamconnect"].(map[string]any)
	if tc == nil || tc["trust"] != "cross-user" {
		t.Fatalf("missing teamconnect meta: %+v", msg.Meta)
	}
	if notice, _ := tc["notice"].(string); !strings.Contains(notice, "危险操作") {
		t.Fatalf("meta notice missing safety wording: %q", notice)
	}
}

func TestInjectReportsRefusalFromPeer(t *testing.T) {
	d := testDaemon(t)
	fp := startFakePeer(t, t.TempDir(), `{"status":"refused","reason":"nope"}`)
	writeKey(t, d.cfg.SessionsDir, 1234, "tok")

	status, _ := d.inject(registryEntry{Pid: 1234, MessagingSocketPath: fp.path}, proto.Frame{MsgID: "m2", Body: "x"})
	if status != proto.StatusRefused {
		t.Fatalf("expected refused, got %s", status)
	}
}

func TestRefusePolicySkipsInjection(t *testing.T) {
	d := testDaemon(t)
	d.inboundPolicy = func() string { return "refuse" }
	status, _ := d.inject(registryEntry{Pid: 1234, MessagingSocketPath: "/does/not/exist"},
		proto.Frame{MsgID: "m3", Body: "x"})
	if status != proto.StatusRefused {
		t.Fatalf("expected refused, got %s", status)
	}
}

func TestInboxModeStoresWithoutInjecting(t *testing.T) {
	d := testDaemon(t)
	d.registry = map[string]registryEntry{"s1": {Pid: 1, SessionID: "s1"}}
	d.sessions = map[string]proto.Session{"s1": {ID: "s1"}}
	d.modes["s1"] = "inbox"

	d.handleDeliver(proto.Frame{MsgID: "m4", ToSession: "s1", Team: "t", Body: "hi",
		From: &proto.Peer{User: "alice", Session: "a1"}})

	entries := d.inbox.list("s1", 10)
	if len(entries) != 1 || entries[0].Body != "hi" || entries[0].Direction != "in" {
		t.Fatalf("inbox entry not stored: %+v", entries)
	}
	if entries[0].Read {
		t.Fatal("inbox entry should start unread")
	}

	unread := d.inbox.drainUnread("s1")
	if len(unread) != 1 {
		t.Fatalf("expected one unread message, got %+v", unread)
	}
	if again := d.inbox.drainUnread("s1"); len(again) != 0 {
		t.Fatalf("drain should mark read, got %+v", again)
	}
}

func TestMuteModeLeavesMessageQueued(t *testing.T) {
	d := testDaemon(t)
	d.registry = map[string]registryEntry{"s1": {Pid: 1, SessionID: "s1"}}
	d.sessions = map[string]proto.Session{"s1": {ID: "s1"}}
	d.modes["s1"] = "mute"

	d.handleDeliver(proto.Frame{MsgID: "m5", ToSession: "s1", Body: "hi"})
	if got := d.inbox.list("s1", 10); len(got) != 0 {
		t.Fatalf("mute should not store messages: %+v", got)
	}
}
