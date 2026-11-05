// Package mcp exposes the AgentWire tools over the Model Context Protocol
// stdio transport, proxying calls to the local daemon.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/dengquanjie/agentwire/internal/daemon"
)

type Server struct {
	Version string
	In      io.Reader
	Out     io.Writer
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) Run() error {
	dec := json.NewDecoder(bufio.NewReader(s.In))
	enc := json.NewEncoder(s.Out)
	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if req.ID == nil {
			// Notification (e.g. notifications/initialized): no reply.
			continue
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = s.initialize(req.Params)
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			resp.Result = map[string]any{"tools": toolDefs()}
		case "tools/call":
			result, err := s.toolsCall(req.Params)
			if err != nil {
				resp.Result = toolError(err.Error())
			} else {
				resp.Result = result
			}
		default:
			resp.Error = &rpcError{Code: -32601, Message: "未知方法：" + req.Method}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := p.ProtocolVersion
	if version == "" {
		version = "2024-11-05"
	}
	ver := s.Version
	if ver == "" {
		ver = "dev"
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "agentwire", "version": ver},
	}
}

func toolDefs() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []map[string]any{
		{
			"name":        "agent_members",
			"description": "列出团队成员及其在线会话（项目、空闲/忙碌、接收模式）。不指定 team 时使用唯一共享 team。",
			"inputSchema": obj(map[string]any{"team": str("team 名称，可选")}),
		},
		{
			"name":        "agent_send",
			"description": "给某位同事发送一条消息，非阻塞，返回 msg_id 与投递状态。",
			"inputSchema": obj(map[string]any{
				"to":      str("收件人用户名"),
				"text":    str("消息正文，最长 8 KB"),
				"team":    str("team 名称；双方只共享一个 team 时可省略"),
				"project": str("目标项目 label；省略则发给对方最近活跃的会话"),
			}, "to", "text"),
		},
		{
			"name":        "agent_broadcast",
			"description": "向 team 内所有在线会话广播一条消息。",
			"inputSchema": obj(map[string]any{
				"text": str("消息正文，最长 8 KB"),
				"team": str("team 名称；只属于一个 team 时可省略"),
			}, "text"),
		},
		{
			"name":        "agent_mode",
			"description": "设置当前会话的接收模式：wake（立即唤醒，默认）、inbox（只收件，不唤醒）、mute（免打扰，服务端排队）。",
			"inputSchema": obj(map[string]any{
				"mode": map[string]any{"type": "string", "enum": []string{"wake", "inbox", "mute"}, "description": "接收模式"},
			}, "mode"),
		},
		{
			"name":        "agent_inbox",
			"description": "查看当前会话在本机保留的收发记录（最近 50 条，不上传服务端）。",
			"inputSchema": obj(map[string]any{}),
		},
	}
}

func (s *Server) toolsCall(params json.RawMessage) (map[string]any, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("参数格式错误")
	}
	args := map[string]any{}
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return nil, fmt.Errorf("参数格式错误")
		}
	}
	get := func(k string) string {
		v, _ := args[k].(string)
		return v
	}
	req := daemon.IPCRequest{
		Session: os.Getenv("CLAUDE_CODE_SESSION_ID"),
		Cwd:     cwd(),
	}
	if pid, err := strconv.Atoi(os.Getenv("CLAUDE_PID")); err == nil {
		req.Pid = pid
	}

	switch p.Name {
	case "agent_members":
		req.Op = "members"
		req.Team = get("team")
	case "agent_send":
		req.Op = "send"
		req.To = get("to")
		req.Text = get("text")
		req.Team = get("team")
		req.Project = get("project")
		if req.To == "" || req.Text == "" {
			return nil, fmt.Errorf("agent_send 需要 to 与 text")
		}
	case "agent_broadcast":
		req.Op = "broadcast"
		req.Text = get("text")
		req.Team = get("team")
		if req.Text == "" {
			return nil, fmt.Errorf("agent_broadcast 需要 text")
		}
	case "agent_mode":
		req.Op = "mode"
		req.Mode = get("mode")
		if req.Mode != "wake" && req.Mode != "inbox" && req.Mode != "mute" {
			return nil, fmt.Errorf("mode 只能是 wake、inbox 或 mute")
		}
	case "agent_inbox":
		req.Op = "inbox"
	default:
		return nil, fmt.Errorf("未知工具：%s", p.Name)
	}

	resp, err := callDaemon(req)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		out := map[string]any{"ok": false, "error": resp.Error}
		if resp.Data != nil {
			out["data"] = resp.Data
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": mustJSON(out)}}, "isError": true}, nil
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": mustJSON(map[string]any{"ok": true, "data": resp.Data})}},
	}, nil
}

func callDaemon(req daemon.IPCRequest) (daemon.IPCResponse, error) {
	conn, err := daemon.DialLocal()
	if err != nil {
		return daemon.IPCResponse{}, fmt.Errorf("连接本机 daemon 失败（请确认 agentwire daemon 正在运行）：%w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return daemon.IPCResponse{}, err
	}
	var resp daemon.IPCResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return daemon.IPCResponse{}, err
	}
	return resp, nil
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"ok":false,"error":"encoding failed"}`
	}
	return string(b)
}

func cwd() string {
	d, err := os.Getwd()
	if err != nil {
		return ""
	}
	return d
}
