package mcp

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dengquanjie/agentwire/internal/daemon"
)

type rpcOut struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func runMCP(t *testing.T, requests ...string) []rpcOut {
	t.Helper()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	out := &bytes.Buffer{}
	if err := (&Server{Version: "test", In: in, Out: out}).Run(); err != nil {
		t.Fatal(err)
	}
	var resps []rpcOut
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var r rpcOut
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad response %q: %v", line, err)
		}
		resps = append(resps, r)
	}
	return resps
}

func TestToolsList(t *testing.T) {
	resps := runMCP(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses (notification must not reply), got %d", len(resps))
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	json.Unmarshal(resps[0].Result, &init)
	if init.ProtocolVersion != "2024-11-05" || init.ServerInfo.Name != "agentwire" {
		t.Fatalf("bad initialize result: %+v", init)
	}

	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(resps[1].Result, &list)
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"agent_members", "agent_send", "agent_broadcast", "agent_mode", "agent_inbox"} {
		if !names[want] {
			t.Fatalf("missing tool %s in %+v", want, names)
		}
	}
}

func TestToolSchemasAreValid(t *testing.T) {
	for _, tool := range toolDefs() {
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("%v: bad inputSchema", tool["name"])
		}
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			t.Fatalf("%v: missing properties", tool["name"])
		}
		if req, ok := schema["required"]; ok {
			reqs, ok := req.([]string)
			if !ok {
				t.Fatalf("%v: required must be an array, got %T", tool["name"], req)
			}
			for _, name := range reqs {
				if _, ok := props[name]; !ok {
					t.Fatalf("%v: required %q has no property", tool["name"], name)
				}
			}
		}
	}

	raw, err := json.Marshal(toolDefs())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"required":null`) {
		t.Fatal("inputSchema must not contain required:null")
	}
}

func TestSendForwardsToDaemonAndReturnsStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-1")
	t.Setenv("CLAUDE_PID", "4242")

	sockDir := filepath.Join(dir, "agentwire")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(sockDir, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	reqs := make(chan daemon.IPCRequest, 4)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			var req daemon.IPCRequest
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				conn.Close()
				continue
			}
			reqs <- req
			json.NewEncoder(conn).Encode(daemon.IPCResponse{
				OK:   true,
				Data: map[string]any{"msg_id": "abc", "status": "delivered"},
			})
			conn.Close()
		}
	}()

	resps := runMCP(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_send","arguments":{"to":"bob","text":"hi","project":"backend"}}}`,
	)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("bad response: %+v", resps)
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(resps[0].Result, &result)
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, `"status":"delivered"`) {
		t.Fatalf("status not returned: %s", result.Content[0].Text)
	}

	select {
	case req := <-reqs:
		if req.Op != "send" || req.To != "bob" || req.Text != "hi" || req.Project != "backend" {
			t.Fatalf("bad IPC request: %+v", req)
		}
		if req.Session != "sess-1" || req.Pid != 4242 {
			t.Fatalf("session identity not forwarded: %+v", req)
		}
	default:
		t.Fatal("daemon was not called")
	}
}

func TestSendMissingArgsReturnsToolError(t *testing.T) {
	resps := runMCP(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_send","arguments":{"text":"hi"}}}`,
	)
	var result struct {
		IsError bool `json:"isError"`
	}
	json.Unmarshal(resps[0].Result, &result)
	if !result.IsError {
		t.Fatalf("expected tool error, got %+v", resps[0].Result)
	}
}

func TestDaemonDownReturnsToolError(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	resps := runMCP(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_inbox","arguments":{}}}`,
	)
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(resps[0].Result, &result)
	if !result.IsError || !strings.Contains(result.Content[0].Text, "daemon") {
		t.Fatalf("expected daemon connection error, got %s", result.Content[0].Text)
	}
}

func TestUnknownMethod(t *testing.T) {
	resps := runMCP(t, `{"jsonrpc":"2.0","id":9,"method":"nope"}`)
	if len(resps) != 1 || resps[0].Error == nil {
		t.Fatalf("expected JSON-RPC error, got %+v", resps)
	}
}
