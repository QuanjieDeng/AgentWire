package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dengquanjie/teamconnect/internal/store"
)

const adminCookie = "tc_session"

type loginState struct {
	count       int
	window      time.Time
	lockedUntil time.Time
}

type adminSession struct {
	userID  int64
	expires time.Time
}

type apiFunc func(http.ResponseWriter, *http.Request, *store.User)

func (h *Hub) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", h.login)
	mux.HandleFunc("POST /api/logout", h.logout)

	// /api/me must stay reachable while a first-login password change is pending.
	mux.HandleFunc("GET /api/me", h.wrap(true, func(w http.ResponseWriter, r *http.Request, u *store.User) { h.me(w, u) }))
	mux.HandleFunc("POST /api/me/password", h.wrap(true, h.changePassword))
	mux.HandleFunc("GET /api/me/tokens", h.wrap(false, h.listTokens))
	mux.HandleFunc("POST /api/me/tokens", h.wrap(false, h.createToken))
	mux.HandleFunc("POST /api/me/tokens/{id}/revoke", h.wrap(false, h.revokeToken))
	mux.HandleFunc("GET /api/me/sessions", h.wrap(false, func(w http.ResponseWriter, r *http.Request, u *store.User) {
		writeJSON(w, 200, map[string]any{"sessions": h.sessionViews(u.ID)})
	}))

	mux.HandleFunc("GET /api/teams", h.wrap(false, h.listTeams))
	mux.HandleFunc("POST /api/teams", h.wrap(false, h.createTeam))
	mux.HandleFunc("POST /api/join", h.wrap(false, h.joinTeam))
	mux.HandleFunc("GET /api/teams/{name}", h.wrap(false, h.teamDetail))
	mux.HandleFunc("POST /api/teams/{name}/invites", h.wrap(false, h.createInvite))
	mux.HandleFunc("POST /api/teams/{name}/leave", h.wrap(false, h.leaveTeam))
	mux.HandleFunc("DELETE /api/teams/{name}/members/{user}", h.wrap(false, h.removeMember))
	mux.HandleFunc("POST /api/teams/{name}/archive", h.wrap(false, h.archiveTeam))
	mux.HandleFunc("GET /api/teams/{name}/stats", h.wrap(false, h.teamStats))

	mux.HandleFunc("GET /api/users", h.wrap(false, h.adminOnly(h.listUsers)))
	mux.HandleFunc("POST /api/users", h.wrap(false, h.adminOnly(h.createUser)))
	mux.HandleFunc("PATCH /api/users/{name}", h.wrap(false, h.adminOnly(h.updateUser)))
	mux.HandleFunc("POST /api/users/{name}/tokens", h.wrap(false, h.adminOnly(h.adminIssueToken)))
	mux.HandleFunc("POST /api/users/{name}/password/reset", h.wrap(false, h.adminOnly(h.adminResetPassword)))
	mux.HandleFunc("GET /api/audit", h.wrap(false, h.adminOnly(h.listAudit)))
	mux.HandleFunc("GET /api/sessions", h.wrap(false, h.adminOnly(h.allSessions)))

	mux.HandleFunc("/", h.serveSPA)
}

func (h *Hub) serveSPA(w http.ResponseWriter, r *http.Request) {
	if h.cfg.WebFS == nil {
		http.Error(w, "Web 后台未构建", http.StatusNotFound)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	if _, err := fs.Stat(h.cfg.WebFS, p); err != nil {
		p = "index.html"
	}
	http.ServeFileFS(w, r, h.cfg.WebFS, p)
}

// ---------- middleware ----------

func (h *Hub) wrap(allowMustChange bool, fn apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.ipAllowed(r.RemoteAddr) {
			writeError(w, http.StatusForbidden, "来源 IP 不在白名单")
			return
		}
		u := h.currentUser(r)
		if u == nil {
			writeError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		if u.MustChangePassword && !allowMustChange {
			writeErrorCode(w, http.StatusForbidden, "password_change_required", "首次登录必须先修改口令")
			return
		}
		fn(w, r, u)
	}
}

func (h *Hub) currentUser(r *http.Request) *store.User {
	c, err := r.Cookie(adminCookie)
	if err != nil {
		return nil
	}
	h.mu.Lock()
	s := h.admins[c.Value]
	if s != nil && time.Now().After(s.expires) {
		delete(h.admins, c.Value)
		s = nil
	}
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	u, err := h.st.UserByID(s.userID)
	if err != nil || u.Status != "active" {
		return nil
	}
	return u
}

func (h *Hub) adminOnly(fn apiFunc) apiFunc {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if u.Role != "admin" {
			writeError(w, http.StatusForbidden, "需要平台管理员权限")
			return
		}
		fn(w, r, u)
	}
}

// ---------- auth ----------

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Hub) login(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if !h.ipAllowed(r.RemoteAddr) {
		writeError(w, 403, "来源 IP 不在白名单")
		return
	}
	if !h.loginAllowed(ip) {
		writeError(w, 429, "登录尝试过多，请 10 分钟后再试")
		return
	}
	var req loginReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, 400, "请求格式错误")
		return
	}
	u, err := h.st.UserByName(strings.TrimSpace(req.Username))
	if err != nil || !store.VerifyPassword(u.PasswordHash, req.Password) {
		h.recordLoginFailure(ip)
		h.st.Audit(0, "login", 0, "", 0, "failed", ip)
		writeError(w, 401, "用户名或口令错误")
		return
	}
	if u.Status != "active" {
		h.recordLoginFailure(ip)
		writeError(w, 403, "账号已被禁用")
		return
	}
	tok := randomToken()
	h.mu.Lock()
	h.admins[tok] = &adminSession{userID: u.ID, expires: time.Now().Add(12 * time.Hour)}
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600,
	})
	h.st.Audit(u.ID, "login", 0, "", 0, "ok", ip)
	writeJSON(w, 200, map[string]any{"user": userJSON(u)})
}

func (h *Hub) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminCookie); err == nil {
		h.mu.Lock()
		delete(h.admins, c.Value)
		h.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *Hub) loginAllowed(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.logins[ip]
	if s == nil {
		return true
	}
	if time.Now().Before(s.lockedUntil) {
		return false
	}
	if time.Since(s.window) > time.Minute {
		delete(h.logins, ip)
	}
	return true
}

func (h *Hub) recordLoginFailure(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.logins[ip]
	if s == nil || time.Since(s.window) > time.Minute {
		s = &loginState{window: time.Now()}
		h.logins[ip] = s
	}
	s.count++
	if s.count >= 5 {
		s.lockedUntil = time.Now().Add(10 * time.Minute)
	}
}

func (h *Hub) me(w http.ResponseWriter, u *store.User) {
	teams, _ := h.st.TeamsForUser(u.ID)
	var out []map[string]any
	for _, t := range teams {
		_, role, _ := h.st.IsMember(t.ID, u.ID)
		members, _ := h.st.TeamMembers(t.ID)
		out = append(out, map[string]any{"id": t.ID, "name": t.Name, "owner": t.OwnerID == u.ID, "role": role, "members": len(members)})
	}
	writeJSON(w, 200, map[string]any{"user": userJSON(u), "teams": out})
}

func (h *Hub) changePassword(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, 400, "请求格式错误")
		return
	}
	if !store.VerifyPassword(u.PasswordHash, req.Old) {
		writeError(w, 401, "原口令错误")
		return
	}
	if len(req.New) < 8 {
		writeError(w, 400, "新口令至少 8 位")
		return
	}
	if err := h.st.SetPassword(u.ID, req.New, false); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	h.st.Audit(u.ID, "password_change", 0, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- tokens ----------

func (h *Hub) listTokens(w http.ResponseWriter, r *http.Request, u *store.User) {
	toks, err := h.st.ListTokens(u.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"tokens": toks})
}

func (h *Hub) createToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	tok, err := h.st.CreateToken(u.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	h.st.Audit(u.ID, "token_create", 0, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"token": tok})
}

func (h *Hub) revokeToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "无效的 token id")
		return
	}
	toks, _ := h.st.ListTokens(u.ID)
	owned := false
	for _, t := range toks {
		if t.ID == id {
			owned = true
		}
	}
	if !owned {
		writeError(w, 404, "token 不存在")
		return
	}
	h.st.RevokeToken(id)
	h.st.Audit(u.ID, "token_revoke", 0, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- teams ----------

func (h *Hub) listTeams(w http.ResponseWriter, r *http.Request, u *store.User) {
	teams, err := h.st.TeamsForUser(u.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	var out []map[string]any
	for _, t := range teams {
		_, role, _ := h.st.IsMember(t.ID, u.ID)
		members, _ := h.st.TeamMembers(t.ID)
		out = append(out, map[string]any{"id": t.ID, "name": t.Name, "owner": t.OwnerID == u.ID, "role": role, "members": len(members)})
	}
	writeJSON(w, 200, map[string]any{"teams": out})
}

func (h *Hub) createTeam(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, 400, "请求格式错误")
		return
	}
	t, err := h.st.CreateTeam(req.Name, u.ID)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, 409, "team 名称已存在")
			return
		}
		writeError(w, 400, err.Error())
		return
	}
	h.st.Audit(u.ID, "team_create", t.ID, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"team": map[string]any{"id": t.ID, "name": t.Name}})
}

func (h *Hub) joinTeam(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, 400, "请求格式错误")
		return
	}
	t, err := h.st.UseInvite(strings.TrimSpace(req.Code), u.ID)
	if err != nil {
		writeError(w, 400, "邀请码无效："+err.Error())
		return
	}
	h.st.Audit(u.ID, "team_join", t.ID, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"team": map[string]any{"id": t.ID, "name": t.Name}})
}

func (h *Hub) teamDetail(w http.ResponseWriter, r *http.Request, u *store.User) {
	t, ok := h.memberTeam(w, r, u)
	if !ok {
		return
	}
	_, role, _ := h.st.IsMember(t.ID, u.ID)
	members, _ := h.st.TeamMembers(t.ID)
	body := map[string]any{
		"team":    map[string]any{"id": t.ID, "name": t.Name, "owner": t.OwnerID == u.ID, "role": role},
		"members": members,
	}
	if role == "owner" || u.Role == "admin" {
		invites, _ := h.st.ListInvites(t.ID)
		stats, _ := h.st.TeamStats(t.ID)
		body["invites"] = invites
		body["stats"] = stats
	}
	writeJSON(w, 200, body)
}

func (h *Hub) createInvite(w http.ResponseWriter, r *http.Request, u *store.User) {
	t, ok := h.ownerTeam(w, r, u)
	if !ok {
		return
	}
	var req struct {
		TTLHours int `json:"ttl_hours"`
		MaxUses  int `json:"max_uses"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req)
	if req.TTLHours <= 0 {
		req.TTLHours = 24 * 7
	}
	if req.MaxUses <= 0 {
		req.MaxUses = 1
	}
	code, err := h.st.CreateInvite(t.ID, u.ID, time.Duration(req.TTLHours)*time.Hour, req.MaxUses)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	h.st.Audit(u.ID, "invite_create", t.ID, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"code": code, "expires_hours": req.TTLHours, "max_uses": req.MaxUses})
}

func (h *Hub) leaveTeam(w http.ResponseWriter, r *http.Request, u *store.User) {
	t, ok := h.memberTeam(w, r, u)
	if !ok {
		return
	}
	if t.OwnerID == u.ID {
		writeError(w, 400, "owner 不能直接退出，请归档 team")
		return
	}
	h.st.RemoveMember(t.ID, u.ID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *Hub) removeMember(w http.ResponseWriter, r *http.Request, u *store.User) {
	t, ok := h.ownerTeam(w, r, u)
	if !ok {
		return
	}
	target, err := h.st.UserByName(r.PathValue("user"))
	if err != nil {
		writeError(w, 404, "用户不存在")
		return
	}
	if target.ID == t.OwnerID {
		writeError(w, 400, "不能移除 owner")
		return
	}
	h.st.RemoveMember(t.ID, target.ID)
	h.st.Audit(u.ID, "member_remove", t.ID, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *Hub) archiveTeam(w http.ResponseWriter, r *http.Request, u *store.User) {
	t, ok := h.ownerTeam(w, r, u)
	if !ok {
		return
	}
	h.st.ArchiveTeam(t.ID)
	h.st.Audit(u.ID, "team_archive", t.ID, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (h *Hub) teamStats(w http.ResponseWriter, r *http.Request, u *store.User) {
	t, ok := h.ownerTeam(w, r, u)
	if !ok {
		return
	}
	stats, err := h.st.TeamStats(t.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"stats": stats})
}

func (h *Hub) memberTeam(w http.ResponseWriter, r *http.Request, u *store.User) (*store.Team, bool) {
	t, err := h.st.TeamByName(r.PathValue("name"))
	if err != nil || t.Status != "active" {
		writeError(w, 404, "team 不存在或已归档")
		return nil, false
	}
	ok, _, _ := h.st.IsMember(t.ID, u.ID)
	if !ok {
		writeError(w, 403, "你不在该 team 中")
		return nil, false
	}
	return t, true
}

func (h *Hub) ownerTeam(w http.ResponseWriter, r *http.Request, u *store.User) (*store.Team, bool) {
	t, ok := h.memberTeam(w, r, u)
	if !ok {
		return nil, false
	}
	if t.OwnerID != u.ID {
		writeError(w, 403, "需要 team owner 权限")
		return nil, false
	}
	return t, true
}

// ---------- admin ----------

func (h *Hub) listUsers(w http.ResponseWriter, r *http.Request, u *store.User) {
	users, err := h.st.ListUsers()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	var out []map[string]any
	for i := range users {
		out = append(out, userJSON(&users[i]))
	}
	writeJSON(w, 200, map[string]any{"users": out})
}

func (h *Hub) createUser(w http.ResponseWriter, r *http.Request, u *store.User) {
	var req struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, 400, "请求格式错误")
		return
	}
	if req.Role == "" {
		req.Role = "member"
	}
	if req.Role != "member" && req.Role != "admin" {
		writeError(w, 400, "角色只能是 member 或 admin")
		return
	}
	pw := randomToken()[:16]
	nu, err := h.st.CreateUser(req.Username, pw, req.Role, true)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, 409, "用户名已存在")
			return
		}
		writeError(w, 400, err.Error())
		return
	}
	h.st.Audit(u.ID, "user_create", 0, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"user": userJSON(nu), "password": pw})
}

func (h *Hub) updateUser(w http.ResponseWriter, r *http.Request, u *store.User) {
	target, err := h.st.UserByName(r.PathValue("name"))
	if err != nil {
		writeError(w, 404, "用户不存在")
		return
	}
	var req struct {
		Status string `json:"status"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, 400, "请求格式错误")
		return
	}
	if req.Status != "" {
		if req.Status != "active" && req.Status != "disabled" {
			writeError(w, 400, "status 只能是 active 或 disabled")
			return
		}
		if target.ID == u.ID && req.Status == "disabled" {
			writeError(w, 400, "不能禁用自己")
			return
		}
		h.st.SetUserStatus(target.ID, req.Status)
	}
	if req.Role != "" {
		if req.Role != "member" && req.Role != "admin" {
			writeError(w, 400, "角色只能是 member 或 admin")
			return
		}
		if target.ID == u.ID && req.Role != "admin" {
			writeError(w, 400, "不能取消自己的管理员角色")
			return
		}
		h.st.SetUserRole(target.ID, req.Role)
	}
	h.st.Audit(u.ID, "user_update", 0, "", 0, "ok", remoteIP(r))
	nt, _ := h.st.UserByID(target.ID)
	writeJSON(w, 200, map[string]any{"user": userJSON(nt)})
}

func (h *Hub) adminIssueToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	target, err := h.st.UserByName(r.PathValue("name"))
	if err != nil {
		writeError(w, 404, "用户不存在")
		return
	}
	tok, err := h.st.CreateToken(target.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	h.st.Audit(u.ID, "token_issue", 0, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"token": tok})
}

func (h *Hub) adminResetPassword(w http.ResponseWriter, r *http.Request, u *store.User) {
	target, err := h.st.UserByName(r.PathValue("name"))
	if err != nil {
		writeError(w, 404, "用户不存在")
		return
	}
	pw := randomToken()[:16]
	if err := h.st.SetPassword(target.ID, pw, true); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	h.st.Audit(u.ID, "password_reset", 0, "", 0, "ok", remoteIP(r))
	writeJSON(w, 200, map[string]any{"password": pw})
}

func (h *Hub) listAudit(w http.ResponseWriter, r *http.Request, u *store.User) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	entries, err := h.st.ListAudit(limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"audit": entries})
}

type sessionView struct {
	User    string `json:"user"`
	Machine string `json:"machine"`
	Session struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Cwd     string `json:"cwd"`
		Project string `json:"project"`
		Status  string `json:"status"`
		Mode    string `json:"mode"`
	} `json:"session"`
}

func (h *Hub) allSessions(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, 200, map[string]any{"sessions": h.sessionViews(0)})
}

// sessionViews returns sessions for one user (0 = everyone, admin view).
func (h *Hub) sessionViews(userID int64) []sessionView {
	h.mu.Lock()
	conns := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		if userID == 0 || c.user.ID == userID {
			conns = append(conns, c)
		}
	}
	h.mu.Unlock()
	var out []sessionView
	for _, c := range conns {
		c.mu.Lock()
		for _, s := range c.sessions {
			v := sessionView{User: c.user.Username, Machine: c.machine}
			v.Session.ID, v.Session.Name, v.Session.Cwd = s.ID, s.Name, s.Cwd
			v.Session.Project, v.Session.Status, v.Session.Mode = s.Project, s.Status, s.Mode
			out = append(out, v)
		}
		c.mu.Unlock()
	}
	return out
}

// ---------- helpers ----------

func userJSON(u *store.User) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role, "status": u.Status,
		"must_change_password": u.MustChangePassword, "created_at": u.CreatedAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeErrorCode(w, status, "", msg)
}

func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	body := map[string]any{"error": msg}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}
