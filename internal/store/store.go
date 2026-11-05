package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrUnauthorized = errors.New("unauthorized")
)

type Store struct{ db *sql.DB }

type User struct {
	ID                 int64
	Username           string
	PasswordHash       string
	Role               string
	Status             string
	MustChangePassword bool
	CreatedAt          int64
}

type Team struct {
	ID        int64
	Name      string
	OwnerID   int64
	Status    string
	CreatedAt int64
}

type Invite struct {
	ExpiresAt int64
	MaxUses   int
	UsedCount int
}

type AuditEntry struct {
	ID       int64  `json:"id"`
	At       int64  `json:"at"`
	Actor    string `json:"actor_user"`
	Action   string `json:"action"`
	TeamID   int64  `json:"team_id"`
	MsgID    string `json:"msg_id"`
	Size     int    `json:"size"`
	Result   string `json:"result"`
	SourceIP string `json:"source_ip"`
}

// Open opens (and migrates) the SQLite database.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single-writer keeps SQLite locking simple; this is a single-instance service.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS users(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'member',
  status TEXT NOT NULL DEFAULT 'active',
  must_change_password INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS user_tokens(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id),
  token_hash TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER,
  last_used_at INTEGER
);
CREATE TABLE IF NOT EXISTS teams(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  owner_user_id INTEGER NOT NULL REFERENCES users(id),
  status TEXT NOT NULL DEFAULT 'active',
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS memberships(
  team_id INTEGER NOT NULL REFERENCES teams(id),
  user_id INTEGER NOT NULL REFERENCES users(id),
  role TEXT NOT NULL DEFAULT 'member',
  joined_at INTEGER NOT NULL,
  PRIMARY KEY(team_id, user_id)
);
CREATE TABLE IF NOT EXISTS invites(
  code_hash TEXT PRIMARY KEY,
  team_id INTEGER NOT NULL REFERENCES teams(id),
  created_by INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  max_uses INTEGER NOT NULL,
  used_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages(
  msg_id TEXT PRIMARY KEY,
  team_id INTEGER NOT NULL,
  from_user INTEGER NOT NULL,
  from_machine TEXT NOT NULL DEFAULT '',
  from_session TEXT NOT NULL DEFAULT '',
  to_user INTEGER,          -- NULL for broadcast
  project TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  delivered_at INTEGER,
  status TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_queue ON messages(to_user, status, expires_at);
CREATE TABLE IF NOT EXISTS deliveries(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  msg_id TEXT NOT NULL REFERENCES messages(msg_id),
  target_user INTEGER NOT NULL,
  target_machine TEXT NOT NULL DEFAULT '',
  target_session TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  delivered_at INTEGER,
  acked_at INTEGER,
  UNIQUE(msg_id, target_user, target_machine, target_session)
);
CREATE TABLE IF NOT EXISTS audit(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at INTEGER NOT NULL,
  actor_user INTEGER,
  action TEXT NOT NULL,
  team_id INTEGER NOT NULL DEFAULT 0,
  msg_id TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  result TEXT NOT NULL,
  source_ip TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit(at);
`)
	return err
}

func now() int64 { return time.Now().Unix() }

// ---------- passwords ----------

func HashPassword(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	h := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("argon2id$19$%s$%s", b64(salt), b64(h))
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "argon2id" {
		return false
	}
	salt, err1 := unb64(parts[2])
	want, err2 := unb64(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func b64(b []byte) string { return hex.EncodeToString(b) }
func unb64(s string) ([]byte, error) {
	return hex.DecodeString(s)
}

// ---------- users ----------

func (s *Store) CreateUser(username, password, role string, mustChange bool) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(password) < 6 {
		return nil, fmt.Errorf("用户名不能为空，口令至少 6 位")
	}
	mc := 0
	if mustChange {
		mc = 1
	}
	res, err := s.db.Exec(`INSERT INTO users(username,password_hash,role,status,must_change_password,created_at)
		VALUES(?,?,?,'active',?,?)`, username, HashPassword(password), role, mc, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrConflict
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(id)
}

func (s *Store) UserByID(id int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id,username,password_hash,role,status,must_change_password,created_at FROM users WHERE id=?`, id))
}

func (s *Store) UserByName(name string) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id,username,password_hash,role,status,must_change_password,created_at FROM users WHERE username=?`, name))
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	var u User
	var mc int
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status, &mc, &u.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.MustChangePassword = mc != 0
	return &u, nil
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id,username,password_hash,role,status,must_change_password,created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var mc int
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status, &mc, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.MustChangePassword = mc != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) SetUserStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE users SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) SetUserRole(id int64, role string) error {
	_, err := s.db.Exec(`UPDATE users SET role=? WHERE id=?`, role, id)
	return err
}

func (s *Store) SetPassword(id int64, password string, mustChange bool) error {
	mc := 0
	if mustChange {
		mc = 1
	}
	_, err := s.db.Exec(`UPDATE users SET password_hash=?, must_change_password=? WHERE id=?`, HashPassword(password), mc, id)
	return err
}

// ---------- tokens ----------

func (s *Store) CreateToken(userID int64) (string, error) {
	tok := randomCode(32)
	_, err := s.db.Exec(`INSERT INTO user_tokens(user_id,token_hash,created_at) VALUES(?,?,?)`,
		userID, hashToken(tok), now())
	return tok, err
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// UserByToken resolves a token to an active user and updates last_used_at.
func (s *Store) UserByToken(tok string) (*User, error) {
	if tok == "" {
		return nil, ErrUnauthorized
	}
	var userID int64
	var revoked sql.NullInt64
	err := s.db.QueryRow(`SELECT user_id,revoked_at FROM user_tokens WHERE token_hash=?`, hashToken(tok)).Scan(&userID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if revoked.Valid {
		return nil, ErrUnauthorized
	}
	u, err := s.UserByID(userID)
	if err != nil {
		return nil, ErrUnauthorized
	}
	if u.Status != "active" {
		return nil, ErrUnauthorized
	}
	s.db.Exec(`UPDATE user_tokens SET last_used_at=? WHERE token_hash=?`, now(), hashToken(tok))
	return u, nil
}

type TokenInfo struct {
	ID         int64  `json:"id"`
	CreatedAt  int64  `json:"created_at"`
	RevokedAt  *int64 `json:"revoked_at,omitempty"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
}

func (s *Store) ListTokens(userID int64) ([]TokenInfo, error) {
	rows, err := s.db.Query(`SELECT id,created_at,revoked_at,last_used_at FROM user_tokens WHERE user_id=? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		var rev, used sql.NullInt64
		if err := rows.Scan(&t.ID, &t.CreatedAt, &rev, &used); err != nil {
			return nil, err
		}
		if rev.Valid {
			t.RevokedAt = &rev.Int64
		}
		if used.Valid {
			t.LastUsedAt = &used.Int64
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeToken(id int64) error {
	_, err := s.db.Exec(`UPDATE user_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now(), id)
	return err
}

func (s *Store) RevokeTokenByValue(tok string) error {
	_, err := s.db.Exec(`UPDATE user_tokens SET revoked_at=? WHERE token_hash=?`, now(), hashToken(tok))
	return err
}

// ---------- teams ----------

func (s *Store) CreateTeam(name string, ownerID int64) (*Team, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("team 名称不能为空")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO teams(name,owner_user_id,status,created_at) VALUES(?,?,'active',?)`, name, ownerID, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrConflict
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO memberships(team_id,user_id,role,joined_at) VALUES(?,?,'owner',?)`, id, ownerID, now()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Team{ID: id, Name: name, OwnerID: ownerID, Status: "active", CreatedAt: now()}, nil
}

func (s *Store) TeamByName(name string) (*Team, error) {
	var t Team
	err := s.db.QueryRow(`SELECT id,name,owner_user_id,status,created_at FROM teams WHERE name=?`, name).
		Scan(&t.ID, &t.Name, &t.OwnerID, &t.Status, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

func (s *Store) TeamByID(id int64) (*Team, error) {
	var t Team
	err := s.db.QueryRow(`SELECT id,name,owner_user_id,status,created_at FROM teams WHERE id=?`, id).
		Scan(&t.ID, &t.Name, &t.OwnerID, &t.Status, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

// TeamsForUser returns active teams the user belongs to.
func (s *Store) TeamsForUser(userID int64) ([]Team, error) {
	rows, err := s.db.Query(`SELECT t.id,t.name,t.owner_user_id,t.status,t.created_at
		FROM teams t JOIN memberships m ON m.team_id=t.id
		WHERE m.user_id=? AND t.status='active' ORDER BY t.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.OwnerID, &t.Status, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SharedTeams returns active teams both users belong to.
func (s *Store) SharedTeams(a, b int64) ([]Team, error) {
	rows, err := s.db.Query(`SELECT t.id,t.name,t.owner_user_id,t.status,t.created_at
		FROM teams t
		JOIN memberships ma ON ma.team_id=t.id AND ma.user_id=?
		JOIN memberships mb ON mb.team_id=t.id AND mb.user_id=?
		WHERE t.status='active' ORDER BY t.name`, a, b)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.OwnerID, &t.Status, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) IsMember(teamID, userID int64) (bool, string, error) {
	var role string
	err := s.db.QueryRow(`SELECT role FROM memberships WHERE team_id=? AND user_id=?`, teamID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, role, nil
}

func (s *Store) AddMember(teamID, userID int64, role string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO memberships(team_id,user_id,role,joined_at) VALUES(?,?,?,?)`,
		teamID, userID, role, now())
	return err
}

func (s *Store) RemoveMember(teamID, userID int64) error {
	_, err := s.db.Exec(`DELETE FROM memberships WHERE team_id=? AND user_id=?`, teamID, userID)
	return err
}

func (s *Store) ArchiveTeam(teamID int64) error {
	_, err := s.db.Exec(`UPDATE teams SET status='archived' WHERE id=?`, teamID)
	return err
}

type Member struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Status   string `json:"status"`
	JoinedAt int64  `json:"joined_at"`
}

func (s *Store) TeamMembers(teamID int64) ([]Member, error) {
	rows, err := s.db.Query(`SELECT u.id,u.username,m.role,u.status,m.joined_at
		FROM memberships m JOIN users u ON u.id=m.user_id
		WHERE m.team_id=? ORDER BY u.username`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Username, &m.Role, &m.Status, &m.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- invites ----------

func (s *Store) CreateInvite(teamID, createdBy int64, ttl time.Duration, maxUses int) (string, error) {
	code := randomCode(16)
	_, err := s.db.Exec(`INSERT INTO invites(code_hash,team_id,created_by,expires_at,max_uses) VALUES(?,?,?,?,?)`,
		hashToken(code), teamID, createdBy, time.Now().Add(ttl).Unix(), maxUses)
	return code, err
}

// UseInvite consumes an invite and adds the user. Returns the team.
func (s *Store) UseInvite(code string, userID int64) (*Team, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var teamID int64
	var expires int64
	var maxUses, used int
	var status string
	err = tx.QueryRow(`SELECT i.team_id,i.expires_at,i.max_uses,i.used_count,t.status
		FROM invites i JOIN teams t ON t.id=i.team_id WHERE i.code_hash=?`, hashToken(code)).
		Scan(&teamID, &expires, &maxUses, &used, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, fmt.Errorf("team 已归档")
	}
	if now() > expires || used >= maxUses {
		return nil, fmt.Errorf("邀请码已过期或已用完")
	}
	if _, err := tx.Exec(`UPDATE invites SET used_count=used_count+1 WHERE code_hash=?`, hashToken(code)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO memberships(team_id,user_id,role,joined_at) VALUES(?,?,'member',?)`,
		teamID, userID, now()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.TeamByID(teamID)
}

type InviteInfo struct {
	ExpiresAt int64 `json:"expires_at"`
	MaxUses   int   `json:"max_uses"`
	UsedCount int   `json:"used_count"`
}

func (s *Store) ListInvites(teamID int64) ([]InviteInfo, error) {
	rows, err := s.db.Query(`SELECT expires_at,max_uses,used_count FROM invites WHERE team_id=? AND expires_at>?`, teamID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InviteInfo
	for rows.Next() {
		var i InviteInfo
		if err := rows.Scan(&i.ExpiresAt, &i.MaxUses, &i.UsedCount); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ---------- messages ----------

type Message struct {
	MsgID       string
	TeamID      int64
	FromUser    int64
	FromMachine string
	FromSession string
	ToUser      *int64
	Project     string
	Body        string
	CreatedAt   int64
	ExpiresAt   int64
	Status      string
}

func (s *Store) SaveMessage(m *Message) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO messages(msg_id,team_id,from_user,from_machine,from_session,to_user,project,body,created_at,expires_at,status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		m.MsgID, m.TeamID, m.FromUser, m.FromMachine, m.FromSession, m.ToUser, m.Project, m.Body, m.CreatedAt, m.ExpiresAt, m.Status)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) MessageByID(msgID string) (*Message, error) {
	var m Message
	var to sql.NullInt64
	err := s.db.QueryRow(`SELECT msg_id,team_id,from_user,from_machine,from_session,to_user,project,body,created_at,expires_at,status
		FROM messages WHERE msg_id=?`, msgID).
		Scan(&m.MsgID, &m.TeamID, &m.FromUser, &m.FromMachine, &m.FromSession, &to, &m.Project, &m.Body, &m.CreatedAt, &m.ExpiresAt, &m.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if to.Valid {
		m.ToUser = &to.Int64
	}
	return &m, nil
}

func (s *Store) SetMessageStatus(msgID, status string, delivered bool) error {
	if delivered {
		_, err := s.db.Exec(`UPDATE messages SET status=?, delivered_at=? WHERE msg_id=?`, status, now(), msgID)
		return err
	}
	_, err := s.db.Exec(`UPDATE messages SET status=? WHERE msg_id=?`, status, msgID)
	return err
}

// QueuedForUser returns unexpired queued messages addressed to user.
func (s *Store) QueuedForUser(userID int64) ([]Message, error) {
	rows, err := s.db.Query(`SELECT msg_id,team_id,from_user,from_machine,from_session,to_user,project,body,created_at,expires_at,status
		FROM messages WHERE to_user=? AND status='queued' AND expires_at>? ORDER BY created_at`, userID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var to sql.NullInt64
		if err := rows.Scan(&m.MsgID, &m.TeamID, &m.FromUser, &m.FromMachine, &m.FromSession, &to, &m.Project, &m.Body, &m.CreatedAt, &m.ExpiresAt, &m.Status); err != nil {
			return nil, err
		}
		if to.Valid {
			m.ToUser = &to.Int64
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ExpiredQueued marks expired queued messages and returns them for sender notification.
func (s *Store) ExpiredQueued() ([]Message, error) {
	rows, err := s.db.Query(`SELECT msg_id,team_id,from_user,from_machine,from_session,to_user,project,body,created_at,expires_at,status
		FROM messages WHERE status='queued' AND expires_at<=?`, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var to sql.NullInt64
		if err := rows.Scan(&m.MsgID, &m.TeamID, &m.FromUser, &m.FromMachine, &m.FromSession, &to, &m.Project, &m.Body, &m.CreatedAt, &m.ExpiresAt, &m.Status); err != nil {
			return nil, err
		}
		if to.Valid {
			m.ToUser = &to.Int64
		}
		out = append(out, m)
	}
	// Close the result set before writing: the pool has one connection.
	rows.Close()
	for _, m := range out {
		s.db.Exec(`UPDATE messages SET status='expired' WHERE msg_id=?`, m.MsgID)
		s.db.Exec(`UPDATE deliveries SET status='expired', acked_at=? WHERE msg_id=? AND status IN ('queued','dispatched')`, now(), m.MsgID)
	}
	return out, nil
}

// Purge removes delivered bodies after 24h and old finished rows.
func (s *Store) Purge(deliveredBodyTTL time.Duration, keepDays int) error {
	cutoff := time.Now().Add(-deliveredBodyTTL).Unix()
	if _, err := s.db.Exec(`UPDATE messages SET body='' WHERE status IN ('delivered','held') AND delivered_at IS NOT NULL AND delivered_at<? AND body<>''`, cutoff); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM deliveries WHERE msg_id IN (SELECT msg_id FROM messages WHERE created_at<?)`, time.Now().AddDate(0, 0, -keepDays).Unix()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM messages WHERE created_at<?`, time.Now().AddDate(0, 0, -keepDays).Unix())
	return err
}

// ---------- deliveries ----------

func (s *Store) AddDelivery(msgID string, targetUser int64, machine, session, status string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO deliveries(msg_id,target_user,target_machine,target_session,status,delivered_at,acked_at)
		VALUES(?,?,?,?,?,?,NULL)`, msgID, targetUser, machine, session, status, now())
	return err
}

// SetDeliveryStatus updates one delivery and reports how many remain non-terminal.
func (s *Store) SetDeliveryStatus(msgID string, targetUser int64, machine, session, status string) (int, error) {
	_, err := s.db.Exec(`UPDATE deliveries SET status=?, acked_at=? WHERE msg_id=? AND target_user=? AND target_machine=? AND target_session=?`,
		status, now(), msgID, targetUser, machine, session)
	if err != nil {
		return 0, err
	}
	return s.deliveriesFor(msgID, "dispatched", "queued")
}

func (s *Store) deliveriesFor(msgID string, statuses ...string) (int, error) {
	placeholders := strings.Repeat("?,", len(statuses))
	placeholders = strings.TrimSuffix(placeholders, ",")
	args := []any{msgID}
	for _, st := range statuses {
		args = append(args, st)
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE msg_id=? AND status IN (`+placeholders+`)`, args...).Scan(&n)
	return n, err
}

type DeliveryInfo struct {
	TargetUser    int64  `json:"target_user"`
	TargetMachine string `json:"target_machine"`
	TargetSession string `json:"target_session"`
	Status        string `json:"status"`
}

func (s *Store) Deliveries(msgID string) ([]DeliveryInfo, error) {
	rows, err := s.db.Query(`SELECT target_user,target_machine,target_session,status FROM deliveries WHERE msg_id=?`, msgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliveryInfo
	for rows.Next() {
		var d DeliveryInfo
		if err := rows.Scan(&d.TargetUser, &d.TargetMachine, &d.TargetSession, &d.Status); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- audit ----------

func (s *Store) Audit(actorUser int64, action string, teamID int64, msgID string, size int, result, ip string) error {
	var actor any
	if actorUser != 0 {
		actor = actorUser
	}
	_, err := s.db.Exec(`INSERT INTO audit(at,actor_user,action,team_id,msg_id,size,result,source_ip) VALUES(?,?,?,?,?,?,?,?)`,
		now(), actor, action, teamID, msgID, size, result, ip)
	return err
}

func (s *Store) ListAudit(limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(`SELECT a.id,a.at,COALESCE(u.username,''),a.action,a.team_id,a.msg_id,a.size,a.result,a.source_ip
		FROM audit a LEFT JOIN users u ON u.id=a.actor_user ORDER BY a.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.TeamID, &e.MsgID, &e.Size, &e.Result, &e.SourceIP); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type TeamStats struct {
	Sent      int `json:"sent"`
	Delivered int `json:"delivered"`
	Queued    int `json:"queued"`
	Failed    int `json:"failed"`
}

func (s *Store) TeamStats(teamID int64) (TeamStats, error) {
	var st TeamStats
	err := s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status IN ('delivered','held','dispatched') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='queued' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status IN ('expired','refused','error') THEN 1 ELSE 0 END),0)
		FROM messages WHERE team_id=? AND to_user IS NOT NULL`, teamID).
		Scan(&st.Sent, &st.Delivered, &st.Queued, &st.Failed)
	return st, err
}

func (s *Store) PurgeAudit(days int) error {
	_, err := s.db.Exec(`DELETE FROM audit WHERE at<?`, time.Now().AddDate(0, 0, -days).Unix())
	return err
}

// ---------- helpers ----------

func randomCode(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
