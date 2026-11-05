package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/dengquanjie/agentwire/internal/proto"
)

// IPCRequest is a local request from the MCP bridge to this daemon.
type IPCRequest struct {
	Op      string `json:"op"`
	Session string `json:"session,omitempty"`
	Pid     int    `json:"pid,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	To      string `json:"to,omitempty"`
	Text    string `json:"text,omitempty"`
	Team    string `json:"team,omitempty"`
	Project string `json:"project,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

type IPCResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Data  any    `json:"data,omitempty"`
}

func (d *Daemon) serveIPC(ctx context.Context) error {
	path, err := ipcSocketPath()
	if err != nil {
		return err
	}
	l, err := listenIPC(path)
	if err != nil {
		return err
	}
	defer l.Close()
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	log.Printf("本机 IPC 就绪：%s", path)
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !peerAllowed(conn) {
			conn.Close()
			continue
		}
		go d.handleIPCConn(conn)
	}
}

func (d *Daemon) handleIPCConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req IPCRequest
		if err := dec.Decode(&req); err != nil {
			return
		}
		if err := enc.Encode(d.handleIPC(req)); err != nil {
			return
		}
	}
}

func (d *Daemon) handleIPC(req IPCRequest) IPCResponse {
	sessionID, err := d.resolveSession(req)
	if err != nil && req.Op != "ping" {
		return IPCResponse{OK: false, Error: err.Error()}
	}
	switch req.Op {
	case "ping":
		return IPCResponse{OK: true, Data: map[string]any{"ok": true}}
	case "members":
		return d.ipcMembers(req)
	case "send":
		return d.ipcSend(req, sessionID, false)
	case "broadcast":
		return d.ipcSend(req, sessionID, true)
	case "mode":
		if err := d.setMode(sessionID, req.Mode); err != nil {
			return IPCResponse{OK: false, Error: err.Error()}
		}
		return IPCResponse{OK: true, Data: map[string]any{"mode": req.Mode, "session": sessionID}}
	case "inbox":
		return IPCResponse{OK: true, Data: map[string]any{"messages": d.inbox.list(sessionID, 50)}}
	case "drain":
		return IPCResponse{OK: true, Data: map[string]any{"messages": d.inbox.drainUnread(sessionID)}}
	}
	return IPCResponse{OK: false, Error: "未知操作：" + req.Op}
}

func (d *Daemon) resolveSession(req IPCRequest) (string, error) {
	d.mu.Lock()
	empty := len(d.registry) == 0
	d.mu.Unlock()
	if empty {
		d.discover()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if req.Session != "" {
		if _, ok := d.registry[req.Session]; ok {
			return req.Session, nil
		}
	}
	if req.Pid != 0 {
		for id, e := range d.registry {
			if e.Pid == req.Pid {
				return id, nil
			}
		}
	}
	if req.Cwd != "" {
		match := ""
		for id, e := range d.registry {
			if e.Cwd == req.Cwd {
				if match != "" {
					return "", fmt.Errorf("当前目录下有多个 Claude 会话，无法确定身份")
				}
				match = id
			}
		}
		if match != "" {
			return match, nil
		}
	}
	return "", fmt.Errorf("找不到当前 Claude 会话；请确认 daemon 已运行并能读取会话注册表")
}

func (d *Daemon) ipcMembers(req IPCRequest) IPCResponse {
	resp, err := d.request(proto.Frame{Type: "members", MsgID: newMsgID(), Team: req.Team}, 8*time.Second)
	if err != nil {
		return IPCResponse{OK: false, Error: err.Error()}
	}
	if resp.Status == proto.StatusError {
		return IPCResponse{OK: false, Error: resp.Message}
	}
	return IPCResponse{OK: true, Data: map[string]any{"team": resp.Team, "members": resp.Members}}
}

func (d *Daemon) ipcSend(req IPCRequest, sessionID string, broadcast bool) IPCResponse {
	if req.Text == "" {
		return IPCResponse{OK: false, Error: "消息正文不能为空"}
	}
	if len(req.Text) > 8*1024 {
		return IPCResponse{OK: false, Error: "消息正文超过 8 KB"}
	}
	if !broadcast && req.To == "" {
		return IPCResponse{OK: false, Error: "必须指定收件人"}
	}
	msgID := newMsgID()
	typ := "send"
	if broadcast {
		typ = "broadcast"
	}
	f := proto.Frame{Type: typ, MsgID: msgID, Team: req.Team, To: req.To,
		Project: req.Project, Body: req.Text, FromSession: sessionID}
	resp, err := d.request(f, 8*time.Second)
	if err != nil {
		return IPCResponse{OK: false, Error: err.Error()}
	}

	status := resp.Status
	if status == "" {
		status = proto.StatusQueued
	}
	d.inbox.add(InboxEntry{
		At: time.Now().Unix(), MsgID: msgID, Team: req.Team, To: req.To,
		Body: req.Text, Session: sessionID, Direction: "out", Read: true,
	})
	data := map[string]any{
		"msg_id": msgID, "status": status, "delivered_to": resp.DeliveredTo,
		"reason": resp.Reason, "code": resp.Code, "message": resp.Message,
		"projects": resp.Projects,
	}
	if status == proto.StatusError {
		return IPCResponse{OK: false, Error: resp.Message, Data: data}
	}
	if status == proto.StatusQueued && broadcast {
		return IPCResponse{OK: false, Error: resp.Message, Data: data}
	}
	return IPCResponse{OK: true, Data: data}
}

func newMsgID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// DialLocal connects to the local daemon IPC socket (used by the MCP bridge).
func DialLocal() (net.Conn, error) {
	return dialLocalIPC()
}
