// Package hub implements the central AgentWire service: WebSocket daemon
// protocol, routing, presence, offline queue, and the admin HTTP API.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dengquanjie/agentwire/internal/proto"
	"github.com/dengquanjie/agentwire/internal/store"
)

const (
	maxBodyBytes  = 8 * 1024
	ratePerSecond = 5
	sessionTTL    = 30 * time.Second
	queueTTL      = 7 * 24 * time.Hour
)

type Config struct {
	Addr        string
	DBPath      string
	AllowIPs    []string
	Version     string
	WebFS       fs.FS
	AllowAllIPs bool
}

type Hub struct {
	cfg Config
	st  *store.Store

	upgrader websocket.Upgrader

	mu         sync.Mutex
	conns      map[string]*Conn // userID|machine -> connection
	rate       map[int64]*bucket
	logins     map[string]*loginState
	admins     map[string]*adminSession
	dispatched map[string]bool
}

type Conn struct {
	hub     *Hub
	ws      *websocket.Conn
	user    store.User
	machine string
	remote  string

	writeMu  sync.Mutex
	mu       sync.Mutex
	sessions map[string]proto.Session
	lastSeen time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func New(cfg Config, st *store.Store) *Hub {
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	return &Hub{
		cfg: cfg,
		st:  st,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
		conns:      map[string]*Conn{},
		rate:       map[int64]*bucket{},
		logins:     map[string]*loginState{},
		admins:     map[string]*adminSession{},
		dispatched: map[string]bool{},
	}
}

// Handler returns the full HTTP handler: admin API, WebSocket and SPA.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.handleWS)
	h.registerAdmin(mux)
	return mux
}

func (h *Hub) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:              h.cfg.Addr,
		Handler:           h.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go h.maintenance(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("AgentWire hub listening on %s", h.cfg.Addr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ---------- IP allow list ----------

func (h *Hub) ipAllowed(remote string) bool {
	if h.cfg.AllowAllIPs || len(h.cfg.AllowIPs) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, entry := range h.cfg.AllowIPs {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(ip) {
				return true
			}
		} else if e := net.ParseIP(entry); e != nil && e.Equal(ip) {
			return true
		}
	}
	return false
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- websocket ----------

func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	if !h.ipAllowed(r.RemoteAddr) {
		http.Error(w, "来源 IP 不在白名单", http.StatusForbidden)
		return
	}
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))

	var hello proto.Frame
	if err := ws.ReadJSON(&hello); err != nil {
		ws.Close()
		return
	}
	if hello.Type != "hello" {
		writeFrame(ws, proto.Frame{Type: "error", Code: proto.ErrBadRequest, Message: "首帧必须是 hello"})
		ws.Close()
		return
	}
	if hello.ProtocolVersion != 0 && hello.ProtocolVersion != proto.ProtocolVersion {
		writeFrame(ws, proto.Frame{Type: "error", Code: proto.ErrVersionMismatch,
			Message: fmt.Sprintf("协议版本不匹配：客户端 %d，服务端 %d", hello.ProtocolVersion, proto.ProtocolVersion)})
		ws.Close()
		return
	}
	user, err := h.st.UserByToken(hello.Token)
	if err != nil {
		writeFrame(ws, proto.Frame{Type: "error", Code: proto.ErrUnauthorized, Message: "token 无效或已禁用"})
		ws.Close()
		return
	}
	machine := strings.TrimSpace(hello.Machine)
	if machine == "" {
		machine = "unknown"
	}

	c := &Conn{
		hub:      h,
		ws:       ws,
		user:     *user,
		machine:  machine,
		remote:   remoteIP(r),
		sessions: map[string]proto.Session{},
		lastSeen: time.Now(),
	}
	// Send welcome before publishing the conn, so no other goroutine can write
	// to it concurrently.
	if err := c.send(proto.Frame{Type: "welcome", ServerVersion: h.cfg.Version,
		ProtocolVersion: proto.ProtocolVersion, User: user.Username}); err != nil {
		ws.Close()
		return
	}

	h.mu.Lock()
	key := connKey(user.ID, machine)
	old := h.conns[key]
	h.conns[key] = c
	h.mu.Unlock()
	if old != nil {
		// close() takes h.mu itself: never call it while holding the lock.
		old.close()
	}
	h.st.Audit(user.ID, "daemon_connect", 0, "", 0, "ok", c.remote)

	c.readLoop()
}

func connKey(userID int64, machine string) string {
	return fmt.Sprintf("%d|%s", userID, machine)
}

func (c *Conn) readLoop() {
	defer c.close()
	for {
		var f proto.Frame
		_ = c.ws.SetReadDeadline(time.Now().Add(45 * time.Second))
		if err := c.ws.ReadJSON(&f); err != nil {
			return
		}
		c.hub.mu.Lock()
		c.lastSeen = time.Now()
		c.hub.mu.Unlock()
		switch f.Type {
		case "heartbeat":
			c.handleHeartbeat(f.Sessions)
		case "send":
			c.hub.handleSend(c, &f)
		case "broadcast":
			c.hub.handleBroadcast(c, &f)
		case "ack":
			c.hub.handleAck(c, &f)
		case "members":
			c.hub.handleMembers(c, &f)
		}
	}
}

func (c *Conn) close() {
	c.hub.mu.Lock()
	key := connKey(c.user.ID, c.machine)
	if cur := c.hub.conns[key]; cur == c {
		delete(c.hub.conns, key)
	}
	c.hub.mu.Unlock()
	c.ws.Close()
}

func (c *Conn) send(f proto.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// Always refresh the deadline: a deadline left over from an earlier write
	// would make this one fail instantly.
	c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteJSON(f)
}

func writeFrame(ws *websocket.Conn, f proto.Frame) error {
	ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return ws.WriteJSON(f)
}

type sessionRef struct {
	conn *Conn
	sess proto.Session
}

func (c *Conn) handleHeartbeat(sessions []proto.Session) {
	now := time.Now().Unix()
	c.mu.Lock()
	c.sessions = map[string]proto.Session{}
	for _, s := range sessions {
		if s.ID == "" {
			continue
		}
		if s.Mode == "" {
			s.Mode = "wake"
		}
		if s.LastActiveAt == 0 {
			s.LastActiveAt = now
		}
		c.sessions[s.ID] = s
	}
	c.mu.Unlock()
	go c.hub.flushQueue(c.user.ID)
}

// sessionsFor returns online, non-muted sessions of a user.
func (h *Hub) sessionsFor(userID int64) []sessionRef {
	return h.sessions(userID, false)
}

// allSessionsFor returns every online session of a user, including muted ones.
func (h *Hub) allSessionsFor(userID int64) []sessionRef {
	return h.sessions(userID, true)
}

func (h *Hub) sessions(userID int64, includeMuted bool) []sessionRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []sessionRef
	for _, c := range h.conns {
		if c.user.ID != userID {
			continue
		}
		c.mu.Lock()
		for _, s := range c.sessions {
			if s.Mode == "mute" && !includeMuted {
				continue
			}
			out = append(out, sessionRef{conn: c, sess: s})
		}
		c.mu.Unlock()
	}
	return out
}

func (h *Hub) handleMembers(c *Conn, f *proto.Frame) {
	resolve := func() (*store.Team, error) { return h.resolveBroadcastTeam(c, f.Team) }
	team, err := resolve()
	if err != nil {
		c.send(proto.Frame{Type: "members_result", MsgID: f.MsgID, Status: proto.StatusError, Message: err.Error()})
		return
	}
	members, err := h.st.TeamMembers(team.ID)
	if err != nil {
		c.send(proto.Frame{Type: "members_result", MsgID: f.MsgID, Status: proto.StatusError, Message: err.Error()})
		return
	}
	out := make([]proto.MemberInfo, 0, len(members))
	for _, m := range members {
		refs := h.allSessionsFor(m.ID)
		mi := proto.MemberInfo{User: m.Username, Role: m.Role}
		for _, r := range refs {
			mi.Sessions = append(mi.Sessions, r.sess)
		}
		slices.SortFunc(mi.Sessions, func(a, b proto.Session) int { return int(a.LastActiveAt - b.LastActiveAt) })
		out = append(out, mi)
	}
	if err := c.send(proto.Frame{Type: "members_result", MsgID: f.MsgID, Team: team.Name, Members: out}); err != nil {
		log.Printf("members_result 发送失败：%v", err)
	}
}

func projectList(refs []sessionRef) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		if r.sess.Project != "" && !seen[r.sess.Project] {
			seen[r.sess.Project] = true
			out = append(out, r.sess.Project)
		}
	}
	sort.Strings(out)
	return out
}

func pickBest(refs []sessionRef) []sessionRef {
	if len(refs) == 0 {
		return nil
	}
	slices.SortFunc(refs, func(a, b sessionRef) int {
		return int(b.sess.LastActiveAt - a.sess.LastActiveAt)
	})
	return refs[:1]
}

// ---------- rate limit ----------

func (h *Hub) allowRate(userID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.rate[userID]
	now := time.Now()
	if b == nil {
		h.rate[userID] = &bucket{tokens: ratePerSecond - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens = min(ratePerSecond, b.tokens+elapsed*ratePerSecond)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ---------- send routing ----------

func (h *Hub) handleSend(c *Conn, f *proto.Frame) {
	h.processSend(c, f, false)
}

func (h *Hub) handleBroadcast(c *Conn, f *proto.Frame) {
	h.processSend(c, f, true)
}

func (h *Hub) processSend(c *Conn, f *proto.Frame, broadcast bool) {
	if !h.allowRate(c.user.ID) {
		h.audit(c, "send", 0, f.MsgID, len(f.Body), "rate_limited")
		c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusError,
			Code: proto.ErrRateLimited, Message: "发送过快，限制为每秒 5 条"})
		return
	}
	if len(f.Body) == 0 || len(f.Body) > maxBodyBytes {
		c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusError,
			Code: proto.ErrTooLarge, Message: fmt.Sprintf("消息正文长度必须在 1..%d 字节之间", maxBodyBytes)})
		return
	}
	if f.MsgID == "" {
		f.MsgID = newID()
	}

	// Idempotency: a repeated msg_id returns the previous outcome.
	if prev, err := h.st.MessageByID(f.MsgID); err == nil {
		c.send(proto.Frame{Type: "delivery_status", MsgID: prev.MsgID, Status: prev.Status,
			Reason: "重复的 msg_id，已按原结果返回"})
		return
	}

	var team *store.Team
	if broadcast {
		t, err := h.resolveBroadcastTeam(c, f.Team)
		if err != nil {
			h.sendErr(c, f, err)
			return
		}
		team = t
	} else {
		to, err := h.st.UserByName(f.To)
		if err != nil {
			h.sendErr(c, f, fmt.Errorf("找不到用户 %q", f.To))
			return
		}
		if to.ID == c.user.ID {
			h.sendErr(c, f, fmt.Errorf("不能发给自己；请直接使用本机会话"))
			return
		}
		t, err := h.resolveTeam(c.user.ID, to.ID, f.Team)
		if err != nil {
			h.sendErr(c, f, err)
			return
		}
		team = t

		h.dispatchDirect(c, f, team, to)
		return
	}

	h.dispatchBroadcast(c, f, team)
}

func (h *Hub) resolveBroadcastTeam(c *Conn, name string) (*store.Team, error) {
	if name == "" {
		teams, err := h.st.TeamsForUser(c.user.ID)
		if err != nil {
			return nil, err
		}
		if len(teams) == 1 {
			return &teams[0], nil
		}
		if len(teams) == 0 {
			return nil, fmt.Errorf("你还没有加入任何 team")
		}
		return nil, fmt.Errorf("你属于多个 team，请显式指定 team：%s", teamNames(teams))
	}
	t, err := h.st.TeamByName(name)
	if err != nil {
		return nil, fmt.Errorf("找不到 team %q", name)
	}
	if t.Status != "active" {
		return nil, fmt.Errorf("team %q 已归档", name)
	}
	ok, _, err := h.st.IsMember(t.ID, c.user.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("你不属于 team %q", name)
	}
	return t, nil
}

func (h *Hub) resolveTeam(fromID, toID int64, name string) (*store.Team, error) {
	if name != "" {
		t, err := h.st.TeamByName(name)
		if err != nil {
			return nil, fmt.Errorf("找不到 team %q", name)
		}
		if t.Status != "active" {
			return nil, fmt.Errorf("team %q 已归档", name)
		}
		for _, uid := range []int64{fromID, toID} {
			ok, _, err := h.st.IsMember(t.ID, uid)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("消息不能跨 team：有成员不属于 team %q", name)
			}
		}
		return t, nil
	}
	shared, err := h.st.SharedTeams(fromID, toID)
	if err != nil {
		return nil, err
	}
	if len(shared) == 1 {
		return &shared[0], nil
	}
	if len(shared) == 0 {
		return nil, fmt.Errorf("你和对方没有共同的 team")
	}
	return nil, fmt.Errorf("你和对方共享多个 team，请显式指定 team：%s", teamNames(shared))
}

func teamNames(teams []store.Team) string {
	names := make([]string, len(teams))
	for i, t := range teams {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

func (h *Hub) dispatchDirect(c *Conn, f *proto.Frame, team *store.Team, to *store.User) {
	refs := h.sessionsFor(to.ID)
	var matched []sessionRef
	if f.Project != "" {
		for _, r := range refs {
			if r.sess.Project == f.Project {
				matched = append(matched, r)
			}
		}
	} else {
		matched = refs
	}

	msg := &store.Message{
		MsgID: f.MsgID, TeamID: team.ID, FromUser: c.user.ID, FromMachine: c.machine,
		FromSession: f.FromSession, ToUser: &to.ID, Project: f.Project, Body: f.Body,
		CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(queueTTL).Unix(), Status: proto.StatusQueued,
	}
	inserted, err := h.st.SaveMessage(msg)
	if err != nil {
		h.sendErr(c, f, err)
		return
	}
	if !inserted {
		prev, _ := h.st.MessageByID(f.MsgID)
		status := "queued"
		if prev != nil {
			status = prev.Status
		}
		c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: status, Reason: "重复的 msg_id"})
		return
	}

	if len(matched) == 0 {
		if len(refs) > 0 {
			// User is online but on other projects: do not queue, report targets.
			projects := projectList(refs)
			h.st.SetMessageStatus(f.MsgID, proto.StatusError, false)
			h.audit(c, "send", team.ID, f.MsgID, len(f.Body), "no_matching_session")
			c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusError,
				Code: proto.ErrNoMatching, Message: "对方在线，但没有匹配该项目的会话",
				Projects: projects, Reason: "可用的在线项目：" + strings.Join(projects, ", ")})
			return
		}
		// Nobody online: keep queued for later delivery.
		h.audit(c, "send", team.ID, f.MsgID, len(f.Body), "queued")
		c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusQueued,
			Reason: "对方当前不在线，消息已排队（保留 7 天）"})
		return
	}

	target := pickBest(matched)[0]
	if !h.sendToSession(msg, team, target, &proto.Peer{User: c.user.Username, Machine: c.machine, Session: f.FromSession}) {
		// Do not claim delivery we did not achieve: keep it queued for retry.
		h.st.SetMessageStatus(f.MsgID, proto.StatusQueued, false)
		h.audit(c, "send", team.ID, f.MsgID, len(f.Body), "queued")
		c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusQueued,
			Reason: "目标会话暂时不可达，消息已排队"})
		return
	}
	h.st.SetMessageStatus(f.MsgID, "dispatched", false)
	h.audit(c, "send", team.ID, f.MsgID, len(f.Body), "dispatched")
	c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusDelivered,
		DeliveredTo: []proto.Target{{User: to.Username, Machine: target.conn.machine, Session: target.sess.ID}},
		Reason:      "已投递到目标会话"})
}

// sendToSession pushes a deliver frame and records the delivery row.
func (h *Hub) sendToSession(msg *store.Message, team *store.Team, ref sessionRef, from *proto.Peer) bool {
	err := ref.conn.send(proto.Frame{
		Type: "deliver", MsgID: msg.MsgID, Team: team.Name, From: from, ToSession: ref.sess.ID,
		Body: msg.Body, CreatedAt: msg.CreatedAt, Project: msg.Project,
	})
	if err != nil {
		return false
	}
	h.st.AddDelivery(msg.MsgID, ref.conn.user.ID, ref.conn.machine, ref.sess.ID, "dispatched")
	return true
}

// claim marks a queued message in flight so concurrent flushes of the same
// user cannot deliver it twice. Callers must unclaim when done.
func (h *Hub) claim(msgID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dispatched[msgID] {
		return false
	}
	h.dispatched[msgID] = true
	return true
}

func (h *Hub) unclaim(msgID string) {
	h.mu.Lock()
	delete(h.dispatched, msgID)
	h.mu.Unlock()
}

func (h *Hub) dispatchBroadcast(c *Conn, f *proto.Frame, team *store.Team) {
	members, err := h.st.TeamMembers(team.ID)
	if err != nil {
		h.sendErr(c, f, err)
		return
	}
	msg := &store.Message{
		MsgID: f.MsgID, TeamID: team.ID, FromUser: c.user.ID, FromMachine: c.machine,
		FromSession: f.FromSession, Project: f.Project, Body: f.Body,
		CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(queueTTL).Unix(), Status: proto.StatusDelivered,
	}
	inserted, err := h.st.SaveMessage(msg)
	if err != nil {
		h.sendErr(c, f, err)
		return
	}
	if !inserted {
		c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusDelivered, Reason: "重复的 msg_id"})
		return
	}

	var targets []proto.Target
	for _, m := range members {
		if m.ID == c.user.ID || m.Status != "active" {
			continue
		}
		for _, r := range h.sessionsFor(m.ID) {
			if h.sendToSession(msg, team, r, &proto.Peer{User: c.user.Username, Machine: c.machine, Session: f.FromSession}) {
				targets = append(targets, proto.Target{User: m.Username, Machine: r.conn.machine, Session: r.sess.ID})
			}
		}
	}
	if len(targets) == 0 {
		h.st.SetMessageStatus(f.MsgID, proto.StatusError, false)
		h.audit(c, "broadcast", team.ID, f.MsgID, len(f.Body), "no_online_session")
		c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusError,
			Code: proto.ErrNoMatching, Message: "team 内当前没有在线会话，广播未投递"})
		return
	}
	h.st.SetMessageStatus(f.MsgID, proto.StatusDelivered, true)
	h.audit(c, "broadcast", team.ID, f.MsgID, len(f.Body), "delivered")
	c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusDelivered, DeliveredTo: targets})
}

func (h *Hub) handleAck(c *Conn, f *proto.Frame) {
	status := f.Status
	if status == "" {
		status = proto.StatusDelivered
	}
	// The ack identifies the session that handled it; fall back to the acking conn.
	n, err := h.st.SetDeliveryStatus(f.MsgID, c.user.ID, c.machine, f.FromSession, status)
	if err != nil {
		log.Printf("ack: %v", err)
		return
	}
	msg, _ := h.st.MessageByID(f.MsgID)
	// A broadcast has one status row but many recipients; a single ack must not
	// stamp the whole broadcast.
	if msg != nil && msg.ToUser != nil {
		h.st.SetMessageStatus(f.MsgID, status, status == proto.StatusDelivered || status == proto.StatusHeld)
	}
	h.st.Audit(c.user.ID, "ack", 0, f.MsgID, 0, status, c.remote)

	if n == 0 && msg != nil {
		// Last delivery resolved; tell the sender what really happened.
		h.notifySender(msg, proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: status,
			Reason: f.Reason, DeliveredTo: deliveryTargets(h, f.MsgID), FromSession: msg.FromSession})
	}
}

func deliveryTargets(h *Hub, msgID string) []proto.Target {
	ds, err := h.st.Deliveries(msgID)
	if err != nil {
		return nil
	}
	var out []proto.Target
	for _, d := range ds {
		name := fmt.Sprint(d.TargetUser)
		if u, err := h.st.UserByID(d.TargetUser); err == nil {
			name = u.Username
		}
		out = append(out, proto.Target{User: name, Machine: d.TargetMachine, Session: d.TargetSession})
	}
	return out
}

func (h *Hub) notifySender(msg *store.Message, f proto.Frame) {
	sender, err := h.st.UserByID(msg.FromUser)
	if err != nil {
		return
	}
	h.mu.Lock()
	var conns []*Conn
	for _, c := range h.conns {
		if c.user.ID == sender.ID {
			conns = append(conns, c)
		}
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.send(f)
	}
}

// flushQueue delivers queued messages that now have a matching online session.
func (h *Hub) flushQueue(userID int64) {
	msgs, err := h.st.QueuedForUser(userID)
	if err != nil {
		return
	}
	if len(msgs) == 0 {
		return
	}
	refs := h.sessionsFor(userID)
	if len(refs) == 0 {
		return
	}
	for i := range msgs {
		m := &msgs[i]
		if !h.claim(m.MsgID) {
			continue
		}
		matched := refs
		if m.Project != "" {
			matched = nil
			for _, r := range refs {
				if r.sess.Project == m.Project {
					matched = append(matched, r)
				}
			}
		}
		if len(matched) == 0 {
			h.unclaim(m.MsgID)
			continue
		}
		team, err := h.st.TeamByID(m.TeamID)
		if err != nil {
			h.unclaim(m.MsgID)
			continue
		}
		target := pickBest(matched)[0]
		from, _ := h.st.UserByID(m.FromUser)
		name := "unknown"
		if from != nil {
			name = from.Username
		}
		sent := h.sendToSession(m, team, target, &proto.Peer{User: name, Machine: m.FromMachine, Session: m.FromSession})
		if sent {
			h.st.SetMessageStatus(m.MsgID, "dispatched", false)
		}
		h.unclaim(m.MsgID)
		if sent {
			h.notifySender(m, proto.Frame{Type: "delivery_status", MsgID: m.MsgID, Status: proto.StatusDelivered,
				FromSession: m.FromSession, Reason: "对方上线，排队消息已补投"})
		}
	}
}

// ---------- maintenance ----------

func (h *Hub) maintenance(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	lastExpire := time.Time{}
	lastPurge := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			h.mu.Lock()
			for _, c := range h.conns {
				if now.Sub(c.lastSeen) > sessionTTL {
					go c.close()
				}
			}
			h.mu.Unlock()
			if now.Sub(lastExpire) > time.Minute {
				lastExpire = now
				h.expireQueued()
			}
			if now.Sub(lastPurge) > time.Hour {
				lastPurge = now
				h.st.Purge(24*time.Hour, 30)
				h.st.PurgeAudit(180)
			}
		}
	}
}

func (h *Hub) expireQueued() {
	msgs, err := h.st.ExpiredQueued()
	if err != nil {
		return
	}
	for _, m := range msgs {
		h.st.Audit(m.FromUser, "expire", m.TeamID, m.MsgID, len(m.Body), "expired", "")
		h.notifySender(&m, proto.Frame{Type: "delivery_status", MsgID: m.MsgID, Status: proto.StatusExpired,
			FromSession: m.FromSession, Reason: "消息超过 7 天未投递，已过期"})
	}
}

// ---------- helpers ----------

func (h *Hub) sendErr(c *Conn, f *proto.Frame, err error) {
	code := proto.ErrBadRequest
	msg := err.Error()
	switch {
	case strings.Contains(msg, "找不到用户"):
		code = proto.ErrNotFound
	case strings.Contains(msg, "跨 team"):
		code = proto.ErrForbidden
	case strings.Contains(msg, "team"):
		code = proto.ErrNoTeam
	}
	c.send(proto.Frame{Type: "delivery_status", MsgID: f.MsgID, Status: proto.StatusError, Code: code, Message: msg})
}

func (h *Hub) audit(c *Conn, action string, teamID int64, msgID string, size int, result string) {
	h.st.Audit(c.user.ID, action, teamID, msgID, size, result, c.remote)
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
