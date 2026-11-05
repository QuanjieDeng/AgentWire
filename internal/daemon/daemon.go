// Package daemon implements the per-user local bridge: session discovery,
// message injection, local inbox and the local IPC socket used by the MCP bridge.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dengquanjie/teamconnect/internal/proto"
)

type Config struct {
	Server      string `json:"server"`
	Token       string `json:"token"`
	Machine     string `json:"machine"`
	Version     string `json:"-"`
	SessionsDir string `json:"-"`
	StateDir    string `json:"-"`
}

func DefaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "teamconnect", "config.json")
}

func DefaultStateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "teamconnect")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "teamconnect-state")
	}
	return filepath.Join(home, ".local", "state", "teamconnect")
}

func DefaultSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".claude", "sessions")
	}
	return filepath.Join(home, ".claude", "sessions")
}

func LoadConfig() (Config, error) {
	b, err := os.ReadFile(DefaultConfigPath())
	if err != nil {
		return Config{}, fmt.Errorf("读取配置失败（先运行 teamconnect login）：%w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Machine == "" {
		h, _ := os.Hostname()
		cfg.Machine = h
	}
	if cfg.SessionsDir == "" {
		cfg.SessionsDir = DefaultSessionsDir()
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir()
	}
	return cfg, nil
}

func SaveConfig(cfg Config) error {
	p := DefaultConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

type Daemon struct {
	cfg Config

	mu       sync.Mutex
	modes    map[string]string
	sessions map[string]proto.Session
	registry map[string]registryEntry
	pending  map[string]chan proto.Frame

	inbox *inbox

	ws      *websocket.Conn
	writeMu sync.Mutex

	projects      map[string]string
	inboundPolicy func() string
}

func New(cfg Config) *Daemon {
	if cfg.SessionsDir == "" {
		cfg.SessionsDir = DefaultSessionsDir()
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir()
	}
	return &Daemon{
		cfg:           cfg,
		modes:         loadModes(cfg.StateDir),
		sessions:      map[string]proto.Session{},
		registry:      map[string]registryEntry{},
		pending:       map[string]chan proto.Frame{},
		inbox:         newInbox(cfg.StateDir),
		projects:      map[string]string{},
		inboundPolicy: crossSessionInbound,
	}
}

func (d *Daemon) Run(ctx context.Context) error {
	go func() {
		if err := d.serveIPC(ctx); err != nil && ctx.Err() == nil {
			log.Printf("本机 IPC 服务退出：%v", err)
		}
	}()
	d.selfCheck()

	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := d.connectAndServe(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			backoff = time.Second
		}
		log.Printf("与中心服务断开：%v；%s 后重连", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

func (d *Daemon) selfCheck() {
	usable := len(d.discover())
	if usable == 0 {
		log.Printf("警告：未发现可注入的 Claude 会话通道（%s）。请确认 Claude Code 正在运行并已启用会话消息。", d.cfg.SessionsDir)
	} else {
		log.Printf("会话通道自检：发现 %d 个可注入会话", usable)
	}
}

func (d *Daemon) connectAndServe(ctx context.Context) (connected bool, err error) {
	url := wsURL(d.cfg.Server)
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return false, err
	}
	defer ws.Close()

	if err := d.writeFrame(ws, proto.Frame{Type: "hello", Token: d.cfg.Token, Machine: d.cfg.Machine,
		DaemonVersion: d.cfg.Version, ProtocolVersion: proto.ProtocolVersion}); err != nil {
		return false, err
	}
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var reply proto.Frame
	if err := ws.ReadJSON(&reply); err != nil {
		return false, err
	}
	if reply.Type == "error" {
		return false, fmt.Errorf("中心服务拒绝连接：%s %s", reply.Code, reply.Message)
	}
	if reply.Type != "welcome" {
		return false, fmt.Errorf("中心服务返回了意外首帧 %q", reply.Type)
	}
	_ = ws.SetReadDeadline(time.Time{})
	connected = true
	log.Printf("已连接中心服务 %s（用户 %s，机器 %s）", d.cfg.Server, reply.User, d.cfg.Machine)

	d.mu.Lock()
	d.ws = ws
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		if d.ws == ws {
			d.ws = nil
		}
		d.mu.Unlock()
	}()

	d.heartbeat(ws)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				ws.Close()
				return
			case <-t.C:
				d.heartbeat(ws)
			}
		}
	}()

	for {
		var f proto.Frame
		if err := ws.ReadJSON(&f); err != nil {
			return connected, err
		}
		switch f.Type {
		case "deliver":
			// Serial on purpose: preserves sender order per SPEC.
			d.handleDeliver(f)
		case "delivery_status":
			if !d.completePending(f) {
				d.recordReceipt(f)
			}
		case "members_result":
			d.completePending(f)
		case "error":
			log.Printf("中心服务错误：%s %s", f.Code, f.Message)
		}
	}
}

func wsURL(server string) string {
	server = strings.TrimRight(server, "/")
	switch {
	case strings.HasPrefix(server, "ws://"), strings.HasPrefix(server, "wss://"):
		return server + "/ws"
	case strings.HasPrefix(server, "http://"):
		return "ws://" + strings.TrimPrefix(server, "http://") + "/ws"
	case strings.HasPrefix(server, "https://"):
		return "wss://" + strings.TrimPrefix(server, "https://") + "/ws"
	default:
		return "ws://" + server + "/ws"
	}
}

func (d *Daemon) writeFrame(ws *websocket.Conn, f proto.Frame) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteJSON(f)
}

func (d *Daemon) heartbeat(ws *websocket.Conn) {
	sessions := d.discover()
	d.mu.Lock()
	d.sessions = map[string]proto.Session{}
	for _, s := range sessions {
		d.sessions[s.ID] = s
	}
	d.mu.Unlock()
	f := proto.Frame{Type: "heartbeat", Sessions: sessions}
	if err := d.writeFrame(ws, f); err != nil {
		ws.Close()
	}
}

func (d *Daemon) handleDeliver(f proto.Frame) {
	d.mu.Lock()
	sessionID := f.ToSession
	if sessionID == "" && len(d.registry) == 1 {
		for id := range d.registry {
			sessionID = id
		}
	}
	mode := d.modes[sessionID]
	if mode == "" {
		mode = "wake"
	}
	entry, ok := d.registry[sessionID]
	d.mu.Unlock()

	if !ok {
		d.ack(f.MsgID, sessionID, proto.StatusRefused, "目标会话已不存在")
		return
	}

	switch mode {
	case "mute":
		d.ack(f.MsgID, sessionID, proto.StatusQueued, "会话处于免打扰模式，消息留在服务端队列")
		return
	case "inbox":
		d.inbox.add(InboxEntry{
			At: time.Now().Unix(), MsgID: f.MsgID, Team: f.Team, From: peerString(f.From),
			Body: f.Body, Session: sessionID, Direction: "in",
		})
		d.ack(f.MsgID, sessionID, proto.StatusDelivered, "已存入本机收件箱")
		return
	}

	status, reason := d.inject(entry, f)
	d.ack(f.MsgID, sessionID, status, reason)
}

func (d *Daemon) ack(msgID, sessionID, status, reason string) {
	d.mu.Lock()
	ws := d.ws
	d.mu.Unlock()
	if ws == nil {
		return
	}
	if err := d.writeFrame(ws, proto.Frame{Type: "ack", MsgID: msgID, FromSession: sessionID,
		Status: status, Reason: reason}); err != nil {
		log.Printf("发送 ack 失败：%v", err)
	}
}

func peerString(p *proto.Peer) string {
	if p == nil {
		return "unknown"
	}
	if p.Session != "" {
		return p.User + "/" + p.Session
	}
	return p.User
}

// request sends a frame and waits for a response carrying the same msg_id.
func (d *Daemon) request(f proto.Frame, timeout time.Duration) (proto.Frame, error) {
	d.mu.Lock()
	ws := d.ws
	if ws == nil {
		d.mu.Unlock()
		return proto.Frame{}, errors.New("本机 daemon 尚未连接中心服务")
	}
	ch := make(chan proto.Frame, 1)
	d.pending[f.MsgID] = ch
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		delete(d.pending, f.MsgID)
		d.mu.Unlock()
	}()
	if err := d.writeFrame(ws, f); err != nil {
		return proto.Frame{}, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		return proto.Frame{}, fmt.Errorf("等待中心服务响应超时")
	}
}

// completePending wakes an in-flight IPC request. It reports whether one was waiting.
func (d *Daemon) completePending(f proto.Frame) bool {
	d.mu.Lock()
	ch := d.pending[f.MsgID]
	d.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- f:
	default:
	}
	return true
}

// recordReceipt surfaces asynchronous delivery outcomes (queued message was
// delivered later, or expired) to the sending session's local inbox.
func (d *Daemon) recordReceipt(f proto.Frame) {
	session := f.FromSession
	if session == "" {
		d.mu.Lock()
		if len(d.registry) == 1 {
			for id := range d.registry {
				session = id
			}
		}
		d.mu.Unlock()
	}
	reason := f.Reason
	if reason == "" {
		reason = f.Message
	}
	log.Printf("投递回执 %s：%s（msg_id=%s）", f.Status, reason, f.MsgID)
	d.inbox.add(InboxEntry{
		At: time.Now().Unix(), MsgID: f.MsgID, Body: "投递回执：" + f.Status + " " + reason,
		Session: session, Direction: "system", Read: true,
	})
}

// ---------- session discovery ----------

type registryEntry struct {
	Pid                 int    `json:"pid"`
	SessionID           string `json:"sessionId"`
	Cwd                 string `json:"cwd"`
	Version             string `json:"version"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	MessagingSocketPath string `json:"messagingSocketPath"`
	UpdatedAt           int64  `json:"updatedAt"`
}

func (d *Daemon) discover() []proto.Session {
	entries := d.readRegistry()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.registry = map[string]registryEntry{}
	out := make([]proto.Session, 0, len(entries))
	for _, e := range entries {
		if e.SessionID == "" || e.MessagingSocketPath == "" {
			continue
		}
		if !processAlive(e.Pid) {
			continue
		}
		d.registry[e.SessionID] = e
		mode := d.modes[e.SessionID]
		if mode == "" {
			mode = "wake"
		}
		name := e.Name
		if name == "" {
			name = "session-" + shortID(e.SessionID)
		}
		status := e.Status
		if status == "" {
			status = "idle"
		}
		out = append(out, proto.Session{
			ID: e.SessionID, Pid: e.Pid, Name: name, Cwd: e.Cwd,
			Project: d.projectLabelLocked(e.Cwd), Status: status, Mode: mode,
			LastActiveAt: e.UpdatedAt / 1000,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (d *Daemon) readRegistry() []registryEntry {
	ents, err := os.ReadDir(d.cfg.SessionsDir)
	if err != nil {
		return nil
	}
	var out []registryEntry
	for _, de := range ents {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d.cfg.SessionsDir, de.Name()))
		if err != nil {
			continue
		}
		var e registryEntry
		if json.Unmarshal(b, &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (d *Daemon) projectLabelLocked(cwd string) string {
	if cwd == "" {
		return ""
	}
	if v, ok := d.projects[cwd]; ok {
		return v
	}
	label := filepath.Base(cwd)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel")
	if out, err := cmd.Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			label = filepath.Base(root)
		}
	}
	d.projects[cwd] = label
	return label
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func loadModes(stateDir string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(stateDir, "modes.json"))
	if err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

func (d *Daemon) saveModes() {
	b, _ := json.MarshalIndent(d.modes, "", "  ")
	os.MkdirAll(d.cfg.StateDir, 0o700)
	os.WriteFile(filepath.Join(d.cfg.StateDir, "modes.json"), b, 0o600)
}

func (d *Daemon) setMode(sessionID, mode string) error {
	if mode != "wake" && mode != "inbox" && mode != "mute" {
		return fmt.Errorf("接收模式只能是 wake、inbox 或 mute")
	}
	d.mu.Lock()
	d.modes[sessionID] = mode
	d.saveModes()
	ws := d.ws
	d.mu.Unlock()
	if ws != nil {
		d.heartbeat(ws)
	}
	return nil
}
