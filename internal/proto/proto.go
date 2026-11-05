// Package proto defines the wire protocol shared by the hub, daemon and MCP bridge.
package proto

// ProtocolVersion is bumped whenever the daemon<->hub framing changes.
const ProtocolVersion = 1

// Frame is a hub<->daemon WebSocket frame. Type selects which fields are used.
type Frame struct {
	Type string `json:"type"`

	// hello / welcome
	Token           string `json:"token,omitempty"`
	Machine         string `json:"machine,omitempty"`
	DaemonVersion   string `json:"daemon_version,omitempty"`
	ProtocolVersion int    `json:"protocol_version,omitempty"`
	ServerVersion   string `json:"server_version,omitempty"`
	User            string `json:"user,omitempty"`

	// heartbeat
	Sessions []Session `json:"sessions,omitempty"`

	// send / deliver / ack / delivery_status
	MsgID       string       `json:"msg_id,omitempty"`
	Team        string       `json:"team,omitempty"`
	To          string       `json:"to,omitempty"`
	Project     string       `json:"project,omitempty"`
	Body        string       `json:"body,omitempty"`
	FromSession string       `json:"from_session,omitempty"`
	ToSession   string       `json:"to_session,omitempty"`
	From        *Peer        `json:"from,omitempty"`
	CreatedAt   int64        `json:"created_at,omitempty"`
	Status      string       `json:"status,omitempty"`
	DeliveredTo []Target     `json:"delivered_to,omitempty"`
	Reason      string       `json:"reason,omitempty"`
	Projects    []string     `json:"projects,omitempty"`
	Members     []MemberInfo `json:"members,omitempty"`

	// error
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Session is one Claude session reported by a daemon.
type Session struct {
	ID           string `json:"id"`             // Claude sessionId
	Pid          int    `json:"-"`              // local bookkeeping only, never sent to teammates
	Name         string `json:"name,omitempty"` // Claude-generated session name
	Cwd          string `json:"cwd,omitempty"`
	Project      string `json:"project,omitempty"`
	Status       string `json:"status,omitempty"` // idle | busy
	Mode         string `json:"mode,omitempty"`   // wake | inbox | mute
	LastActiveAt int64  `json:"last_active_at,omitempty"`
}

// MemberInfo is one team member and their online sessions.
type MemberInfo struct {
	User     string    `json:"user"`
	Role     string    `json:"role,omitempty"`
	Sessions []Session `json:"sessions,omitempty"`
}

// Peer identifies the origin of a message.
type Peer struct {
	User    string `json:"user"`
	Machine string `json:"machine,omitempty"`
	Session string `json:"session,omitempty"`
}

// Target identifies one delivery destination.
type Target struct {
	User    string `json:"user"`
	Machine string `json:"machine,omitempty"`
	Session string `json:"session,omitempty"`
}

// Delivery statuses.
const (
	StatusDelivered = "delivered"
	StatusQueued    = "queued"
	StatusHeld      = "held"
	StatusRefused   = "refused"
	StatusExpired   = "expired"
	StatusError     = "error"
	StatusMuted     = "muted"
)

// Error codes.
const (
	ErrVersionMismatch = "version_mismatch"
	ErrUnauthorized    = "unauthorized"
	ErrRateLimited     = "rate_limited"
	ErrTooLarge        = "too_large"
	ErrNoTeam          = "team_required"
	ErrNoMatching      = "no_matching_session"
	ErrNotFound        = "not_found"
	ErrBadRequest      = "bad_request"
	ErrForbidden       = "forbidden"
)
