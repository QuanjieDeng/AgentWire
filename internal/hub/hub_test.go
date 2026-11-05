package hub_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dengquanjie/agentwire/internal/hub"
	"github.com/dengquanjie/agentwire/internal/proto"
	"github.com/dengquanjie/agentwire/internal/store"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(hub.Config{Version: "test", AllowAllIPs: true}, st)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		srv.Close()
		st.Close()
	})
	return &env{t: t, srv: srv, st: st}
}

func (e *env) userWithToken(name string) (*store.User, string) {
	e.t.Helper()
	u, err := e.st.CreateUser(name, "password-"+name, "member", false)
	if err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.st.CreateToken(u.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return u, tok
}

func (e *env) adminWithToken(name string) (*store.User, string) {
	e.t.Helper()
	u, err := e.st.CreateUser(name, "password-"+name, "admin", false)
	if err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.st.CreateToken(u.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return u, tok
}

func (e *env) team(name string, users ...*store.User) *store.Team {
	e.t.Helper()
	if len(users) == 0 {
		e.t.Fatal("need an owner")
	}
	tm, err := e.st.CreateTeam(name, users[0].ID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, u := range users[1:] {
		if err := e.st.AddMember(tm.ID, u.ID, "member"); err != nil {
			e.t.Fatal(err)
		}
	}
	return tm
}

type daemonClient struct {
	t  *testing.T
	ws *websocket.Conn
}

func (e *env) connect(token, machine string) *daemonClient {
	e.t.Helper()
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	d := &daemonClient{t: e.t, ws: ws}
	d.send(proto.Frame{Type: "hello", Token: token, Machine: machine, ProtocolVersion: proto.ProtocolVersion})
	f := d.read()
	if f.Type != "welcome" {
		e.t.Fatalf("expected welcome, got %s: %+v", f.Type, f)
	}
	e.t.Cleanup(func() { ws.Close() })
	return d
}

func (d *daemonClient) send(f proto.Frame) {
	d.t.Helper()
	if err := d.ws.WriteJSON(f); err != nil {
		d.t.Fatal(err)
	}
}

func (d *daemonClient) read() proto.Frame {
	d.t.Helper()
	var f proto.Frame
	d.ws.SetReadDeadline(time.Now().Add(4 * time.Second))
	if err := d.ws.ReadJSON(&f); err != nil {
		d.t.Fatalf("read frame: %v", err)
	}
	return f
}

// syncHeartbeat blocks until this connection has processed its queued
// heartbeats, making hub ordering deterministic in tests.
func (d *daemonClient) syncHeartbeat(team string) {
	d.t.Helper()
	d.send(proto.Frame{Type: "members", MsgID: "sync-" + d.ws.LocalAddr().String() + team, Team: team})
	for {
		f := d.read()
		if f.Type == "members_result" {
			return
		}
	}
}

func (d *daemonClient) expectNoFrame() {
	d.t.Helper()
	var f proto.Frame
	d.ws.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	err := d.ws.ReadJSON(&f)
	if err == nil {
		d.t.Fatalf("expected no frame, got %+v", f)
	}
}

func session(id, project string) proto.Session {
	return proto.Session{ID: id, Project: project, Name: "name-" + id, Status: "idle", Mode: "wake", LastActiveAt: time.Now().Unix()}
}

func TestDirectDeliveryAndAck(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("proj", alice, bob)

	ad := e.connect(aTok, "m-alice")
	bd := e.connect(bTok, "m-bob")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "proj")}})
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("b1", "proj")}})
	ad.syncHeartbeat("proj")
	bd.syncHeartbeat("proj")

	ad.send(proto.Frame{Type: "send", MsgID: "m1", To: "bob", Body: "hello bob", FromSession: "a1"})

	deliver := bd.read()
	if deliver.Type != "deliver" || deliver.Body != "hello bob" || deliver.ToSession != "b1" {
		t.Fatalf("bad deliver frame: %+v", deliver)
	}
	if deliver.From == nil || deliver.From.User != "alice" || deliver.From.Session != "a1" {
		t.Fatalf("bad from field: %+v", deliver.From)
	}

	sent := ad.read()
	if sent.Type != "delivery_status" || sent.MsgID != "m1" || sent.Status != proto.StatusDelivered {
		t.Fatalf("bad send reply: %+v", sent)
	}
	if len(sent.DeliveredTo) != 1 || sent.DeliveredTo[0].Session != "b1" {
		t.Fatalf("bad delivered_to: %+v", sent.DeliveredTo)
	}

	bd.send(proto.Frame{Type: "ack", MsgID: "m1", FromSession: "b1", Status: proto.StatusDelivered})
	acked := ad.read()
	if acked.Type != "delivery_status" || acked.MsgID != "m1" || acked.Status != proto.StatusDelivered {
		t.Fatalf("bad ack notification: %+v", acked)
	}
}

func TestDuplicateMsgIDIsIdempotent(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("proj", alice, bob)
	ad := e.connect(aTok, "ma")
	bd := e.connect(bTok, "mb")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("b1", "p")}})
	ad.syncHeartbeat("proj")
	bd.syncHeartbeat("proj")

	ad.send(proto.Frame{Type: "send", MsgID: "dup", To: "bob", Body: "one", FromSession: "a1"})
	if f := bd.read(); f.Type != "deliver" {
		t.Fatalf("expected deliver, got %+v", f)
	}
	ad.read() // delivered

	ad.send(proto.Frame{Type: "send", MsgID: "dup", To: "bob", Body: "one", FromSession: "a1"})
	reply := ad.read()
	if reply.MsgID != "dup" || reply.Status != "dispatched" {
		t.Fatalf("duplicate should return previous status, got %+v", reply)
	}
	bd.expectNoFrame()
}

func TestOfflineQueueDeliveredOnHeartbeat(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("proj", alice, bob)
	ad := e.connect(aTok, "ma")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	ad.syncHeartbeat("proj")

	ad.send(proto.Frame{Type: "send", MsgID: "q1", To: "bob", Body: "later", FromSession: "a1"})
	reply := ad.read()
	if reply.Status != proto.StatusQueued {
		t.Fatalf("expected queued, got %+v", reply)
	}

	bd := e.connect(bTok, "mb")
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("b1", "p")}})
	bd.syncHeartbeat("proj")
	deliver := bd.read()
	if deliver.Type != "deliver" || deliver.MsgID != "q1" {
		t.Fatalf("queued message not delivered: %+v", deliver)
	}
}

func TestProjectRoutingPicksMatchingSession(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("proj", alice, bob)
	ad := e.connect(aTok, "ma")
	bd := e.connect(bTok, "mb")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{
		session("b1", "frontend"), session("b2", "backend"),
	}})
	ad.syncHeartbeat("proj")
	bd.syncHeartbeat("proj")

	ad.send(proto.Frame{Type: "send", MsgID: "p1", To: "bob", Project: "backend", Body: "api?", FromSession: "a1"})
	deliver := bd.read()
	if deliver.ToSession != "b2" {
		t.Fatalf("expected session b2, got %+v", deliver)
	}
	ad.read()
}

func TestProjectMismatchReportsOnlineProjects(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("proj", alice, bob)
	ad := e.connect(aTok, "ma")
	bd := e.connect(bTok, "mb")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("b1", "frontend"), session("b2", "backend")}})
	ad.syncHeartbeat("proj")
	bd.syncHeartbeat("proj")

	ad.send(proto.Frame{Type: "send", MsgID: "p2", To: "bob", Project: "mobile", Body: "?", FromSession: "a1"})
	reply := ad.read()
	if reply.Status != proto.StatusError || reply.Code != proto.ErrNoMatching {
		t.Fatalf("expected no_matching error, got %+v", reply)
	}
	if len(reply.Projects) != 2 || reply.Projects[0] != "backend" || reply.Projects[1] != "frontend" {
		t.Fatalf("expected online projects, got %+v", reply.Projects)
	}
}

func TestTeamMustBeExplicitWhenAmbiguous(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("one", alice, bob)
	e.team("two", alice, bob)
	ad := e.connect(aTok, "ma")
	e.connect(bTok, "mb")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	ad.syncHeartbeat("one")

	ad.send(proto.Frame{Type: "send", MsgID: "amb", To: "bob", Body: "x", FromSession: "a1"})
	reply := ad.read()
	if reply.Status != proto.StatusError || reply.Code != proto.ErrNoTeam {
		t.Fatalf("expected team_required, got %+v", reply)
	}
	if !strings.Contains(reply.Message, "one") || !strings.Contains(reply.Message, "two") {
		t.Fatalf("expected team names in error, got %q", reply.Message)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("proj", alice, bob)
	ad := e.connect(aTok, "ma")
	bd := e.connect(bTok, "mb")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("b1", "p")}})
	ad.syncHeartbeat("proj")
	bd.syncHeartbeat("proj")

	for i := 0; i < 5; i++ {
		ad.send(proto.Frame{Type: "send", MsgID: "rl" + string(rune('a'+i)), To: "bob", Body: "x", FromSession: "a1"})
	}
	ad.send(proto.Frame{Type: "send", MsgID: "rl-fail", To: "bob", Body: "x", FromSession: "a1"})
	var reply proto.Frame
	for i := 0; i < 6; i++ {
		reply = ad.read()
	}
	if reply.Code != proto.ErrRateLimited {
		t.Fatalf("expected rate limit, got %+v", reply)
	}
}

func TestVersionMismatchIsExplicit(t *testing.T) {
	e := newEnv(t)
	_, tok := e.userWithToken("alice")
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.WriteJSON(proto.Frame{Type: "hello", Token: tok, Machine: "m", ProtocolVersion: 999})
	var f proto.Frame
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := ws.ReadJSON(&f); err != nil {
		t.Fatal(err)
	}
	if f.Type != "error" || f.Code != proto.ErrVersionMismatch {
		t.Fatalf("expected version_mismatch, got %+v", f)
	}
}

func TestBroadcastFansOut(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	carol, cTok := e.userWithToken("carol")
	e.team("proj", alice, bob, carol)
	ad := e.connect(aTok, "ma")
	bd := e.connect(bTok, "mb")
	cd := e.connect(cTok, "mc")
	for d, id := range map[*daemonClient]string{ad: "a1", bd: "b1", cd: "c1"} {
		d.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session(id, "p")}})
	}
	ad.syncHeartbeat("proj")
	bd.syncHeartbeat("proj")
	cd.syncHeartbeat("proj")

	ad.send(proto.Frame{Type: "broadcast", MsgID: "bc1", Body: "all hands", FromSession: "a1"})
	if f := bd.read(); f.Type != "deliver" || f.MsgID != "bc1" {
		t.Fatalf("bob did not get broadcast: %+v", f)
	}
	if f := cd.read(); f.Type != "deliver" || f.MsgID != "bc1" {
		t.Fatalf("carol did not get broadcast: %+v", f)
	}
	reply := ad.read()
	if reply.Status != proto.StatusDelivered || len(reply.DeliveredTo) != 2 {
		t.Fatalf("bad broadcast reply: %+v", reply)
	}
}

func TestMembersQuery(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("proj", alice, bob)
	ad := e.connect(aTok, "ma")
	bd := e.connect(bTok, "mb")
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("b1", "backend")}})
	bd.syncHeartbeat("proj")

	ad.send(proto.Frame{Type: "members", MsgID: "q", Team: "proj"})
	resp := ad.read()
	if resp.Type != "members_result" || len(resp.Members) != 2 {
		t.Fatalf("bad members result: %+v", resp)
	}
	for _, m := range resp.Members {
		if m.User == "bob" && (len(m.Sessions) != 1 || m.Sessions[0].Project != "backend") {
			t.Fatalf("bob sessions not reported: %+v", m)
		}
	}
}

func TestAdminAuditHasNoBody(t *testing.T) {
	e := newEnv(t)
	admin, _ := e.adminWithToken("root")
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("proj", alice, bob)
	ad := e.connect(aTok, "ma")
	bd := e.connect(bTok, "mb")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("b1", "p")}})
	ad.syncHeartbeat("proj")
	bd.syncHeartbeat("proj")
	ad.send(proto.Frame{Type: "send", MsgID: "secret-msg", To: "bob", Body: "TOP-SECRET-BODY", FromSession: "a1"})
	bd.read()
	ad.read()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/login",
		strings.NewReader(`{"username":"root","password":"password-root"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("admin login failed: %d", resp.StatusCode)
	}
	_ = admin

	resp, err = client.Get(e.srv.URL + "/api/audit")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("audit failed: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Audit []store.AuditEntry `json:"audit"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Audit) == 0 {
		t.Fatal("expected audit entries")
	}
	if strings.Contains(string(body), "TOP-SECRET-BODY") {
		t.Fatal("audit leaked message body")
	}
}

func (e *env) loginClient(username, password string) *http.Client {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := client.Post(e.srv.URL+"/api/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		e.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		e.t.Fatalf("login %s failed: %d", username, resp.StatusCode)
	}
	return client
}

func doJSON(t *testing.T, client *http.Client, method, url, payload string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestFirstLoginMustChangePassword(t *testing.T) {
	e := newEnv(t)
	e.adminWithToken("root")
	admin := e.loginClient("root", "password-root")

	status, body := doJSON(t, admin, "POST", e.srv.URL+"/api/users", `{"username":"newbie","role":"member"}`)
	if status != 200 {
		t.Fatalf("create user: %d %s", status, body)
	}
	var created struct {
		Password string `json:"password"`
	}
	json.Unmarshal(body, &created)
	if created.Password == "" {
		t.Fatal("initial password not returned")
	}

	newbie := e.loginClient("newbie", created.Password)
	// /api/me stays available so the UI can render the forced-change screen.
	status, body = doJSON(t, newbie, "GET", e.srv.URL+"/api/me", "")
	if status != 200 || !strings.Contains(string(body), "must_change_password") {
		t.Fatalf("/api/me should be reachable before password change: %d %s", status, body)
	}
	status, body = doJSON(t, newbie, "GET", e.srv.URL+"/api/teams", "")
	if status != 403 || !strings.Contains(string(body), "password_change_required") {
		t.Fatalf("expected forced password change, got %d %s", status, body)
	}

	status, body = doJSON(t, newbie, "POST", e.srv.URL+"/api/me/password",
		`{"old":"`+created.Password+`","new":"a-new-password"}`)
	if status != 200 {
		t.Fatalf("change password: %d %s", status, body)
	}
	status, body = doJSON(t, newbie, "GET", e.srv.URL+"/api/teams", "")
	if status != 200 {
		t.Fatalf("teams after change: %d %s", status, body)
	}
}

func TestInviteJoinAndOwnerOnly(t *testing.T) {
	e := newEnv(t)
	e.userWithToken("alice")
	e.userWithToken("bob")
	alice := e.loginClient("alice", "password-alice")
	bob := e.loginClient("bob", "password-bob")

	status, body := doJSON(t, alice, "POST", e.srv.URL+"/api/teams", `{"name":"proj"}`)
	if status != 200 {
		t.Fatalf("create team: %d %s", status, body)
	}
	status, body = doJSON(t, alice, "POST", e.srv.URL+"/api/teams/proj/invites", `{"ttl_hours":1,"max_uses":1}`)
	if status != 200 {
		t.Fatalf("create invite: %d %s", status, body)
	}
	var inv struct {
		Code string `json:"code"`
	}
	json.Unmarshal(body, &inv)

	status, body = doJSON(t, bob, "POST", e.srv.URL+"/api/join", `{"code":"`+inv.Code+`"}`)
	if status != 200 {
		t.Fatalf("join: %d %s", status, body)
	}

	// Invite is single-use.
	status, _ = doJSON(t, bob, "POST", e.srv.URL+"/api/join", `{"code":"`+inv.Code+`"}`)
	if status == 200 {
		t.Fatal("single-use invite accepted twice")
	}

	// Only the owner may mint invites.
	status, _ = doJSON(t, bob, "POST", e.srv.URL+"/api/teams/proj/invites", `{"ttl_hours":1,"max_uses":1}`)
	if status != 403 {
		t.Fatalf("member should not mint invites, got %d", status)
	}

	status, body = doJSON(t, bob, "GET", e.srv.URL+"/api/teams", "")
	if status != 200 || !strings.Contains(string(body), `"proj"`) {
		t.Fatalf("bob should see joined team: %d %s", status, body)
	}
}

func TestEmptyListEndpointsReturnArrays(t *testing.T) {
	e := newEnv(t)
	e.userWithToken("solo")
	client := e.loginClient("solo", "password-solo")

	status, body := doJSON(t, client, "GET", e.srv.URL+"/api/teams", "")
	if status != 200 || !strings.Contains(string(body), `"teams":[]`) {
		t.Fatalf("empty teams must be [], got %d %s", status, body)
	}
	status, body = doJSON(t, client, "GET", e.srv.URL+"/api/me/sessions", "")
	if status != 200 || !strings.Contains(string(body), `"sessions":[]`) {
		t.Fatalf("empty sessions must be [], got %d %s", status, body)
	}
}

func TestReplyWorksAfterWelcomeWriteDeadline(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	e.team("proj", alice)
	d := e.connect(aTok, "m")
	// The welcome frame sets a 5s write deadline; a later reply must refresh it.
	time.Sleep(11 * time.Second)
	d.send(proto.Frame{Type: "members", MsgID: "late", Team: "proj"})
	f := d.read()
	if f.Type != "members_result" || f.MsgID != "late" {
		t.Fatalf("reply after 6s failed: %+v", f)
	}
}

func TestArchivedTeamRejectsMessagesAndInvites(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	tm := e.team("proj", alice, bob)
	ad := e.connect(aTok, "ma")
	bd := e.connect(bTok, "mb")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("b1", "p")}})
	ad.syncHeartbeat("proj")
	bd.syncHeartbeat("proj")

	code, err := e.st.CreateInvite(tm.ID, alice.ID, time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.ArchiveTeam(tm.ID); err != nil {
		t.Fatal(err)
	}

	ad.send(proto.Frame{Type: "send", MsgID: "after-archive", To: "bob", Team: "proj", Body: "x", FromSession: "a1"})
	reply := ad.read()
	if reply.Status != proto.StatusError {
		t.Fatalf("archived team should reject messages: %+v", reply)
	}
	if _, err := e.st.UseInvite(code, bob.ID); err == nil {
		t.Fatal("archived team should reject invites")
	}
}

func TestMutedTargetQueuesAndDeliversAfterUnmute(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	bob, bTok := e.userWithToken("bob")
	e.team("proj", alice, bob)
	ad := e.connect(aTok, "ma")
	bd := e.connect(bTok, "mb")
	ad.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	muted := session("b1", "p")
	muted.Mode = "mute"
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{muted}})
	ad.syncHeartbeat("proj")
	bd.syncHeartbeat("proj")

	ad.send(proto.Frame{Type: "send", MsgID: "mute1", To: "bob", Project: "p", Body: "hold", FromSession: "a1"})
	reply := ad.read()
	if reply.Status != proto.StatusQueued {
		t.Fatalf("muted target should queue, got %+v", reply)
	}

	// Unmute: the next heartbeat must flush the queued message.
	bd.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("b1", "p")}})
	for {
		f := bd.read()
		if f.Type == "deliver" {
			if f.MsgID != "mute1" {
				t.Fatalf("unexpected deliver: %+v", f)
			}
			break
		}
	}
}

func TestReconnectSameMachineDoesNotDeadlock(t *testing.T) {
	e := newEnv(t)
	alice, aTok := e.userWithToken("alice")
	e.team("proj", alice)

	// A second daemon on the same machine replaces the first. This must not
	// leave the hub mutex locked.
	e.connect(aTok, "same-machine")
	second := e.connect(aTok, "same-machine")
	second.send(proto.Frame{Type: "heartbeat", Sessions: []proto.Session{session("a1", "p")}})
	second.syncHeartbeat("proj")
}

func TestUnparseableFrameDoesNotPanic(t *testing.T) {
	e := newEnv(t)
	e.userWithToken("alice")
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.WriteMessage(websocket.TextMessage, []byte("{not json"))
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	var f proto.Frame
	_ = ws.ReadJSON(&f)
	// Hub should have closed the connection instead of crashing.
}
