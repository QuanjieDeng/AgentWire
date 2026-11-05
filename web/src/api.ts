export interface User {
  id: number;
  username: string;
  role: "admin" | "member";
  status: string;
  must_change_password: boolean;
  created_at: number;
}

export interface Team {
  id: number;
  name: string;
  role: string;
  owner: boolean;
  members: number;
}

export interface Member {
  id: number;
  username: string;
  role: string;
  status: string;
  joined_at: number;
}

export interface SessionView {
  user: string;
  machine: string;
  session: {
    id: string;
    name: string;
    cwd: string;
    project: string;
    status: string;
    mode: string;
  };
}

export interface AuditEntry {
  id: number;
  at: number;
  actor_user: string;
  action: string;
  team_id: number;
  msg_id: string;
  size: number;
  result: string;
  source_ip: string;
}

export interface TokenInfo {
  id: number;
  created_at: number;
  revoked_at?: number;
  last_used_at?: number;
}

export class ApiError extends Error {
  code?: string;
  status: number;
  constructor(message: string, status: number, code?: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  const text = await res.text();
  const body = text ? JSON.parse(text) : {};
  if (!res.ok) {
    throw new ApiError(body.error || `请求失败（${res.status}）`, res.status, body.code);
  }
  return body as T;
}

export const api = {
  me: () => request<{ user: User; teams: Team[] }>("/api/me"),
  login: (username: string, password: string) =>
    request<{ user: User }>("/api/login", {
      method: "POST",
      body: JSON.stringify({ username, password }),
    }),
  logout: () => request("/api/logout", { method: "POST", body: "{}" }),
  changePassword: (oldPassword: string, newPassword: string) =>
    request("/api/me/password", {
      method: "POST",
      body: JSON.stringify({ old: oldPassword, new: newPassword }),
    }),
  tokens: () => request<{ tokens: TokenInfo[] }>("/api/me/tokens"),
  createToken: () => request<{ token: string }>("/api/me/tokens", { method: "POST", body: "{}" }),
  revokeToken: (id: number) => request(`/api/me/tokens/${id}/revoke`, { method: "POST", body: "{}" }),
  sessions: () => request<{ sessions: SessionView[] }>("/api/me/sessions"),
  teams: () => request<{ teams: Team[] }>("/api/teams"),
  createTeam: (name: string) =>
    request("/api/teams", { method: "POST", body: JSON.stringify({ name }) }),
  joinTeam: (code: string) =>
    request("/api/join", { method: "POST", body: JSON.stringify({ code }) }),
  team: (name: string) =>
    request<{
      team: { id: number; name: string; role: string; owner: boolean };
      members: Member[];
      invites?: { expires_at: number; max_uses: number; used_count: number }[];
      stats?: { sent: number; delivered: number; queued: number; failed: number };
    }>(`/api/teams/${encodeURIComponent(name)}`),
  createInvite: (name: string, ttlHours: number, maxUses: number) =>
    request<{ code: string }>(`/api/teams/${encodeURIComponent(name)}/invites`, {
      method: "POST",
      body: JSON.stringify({ ttl_hours: ttlHours, max_uses: maxUses }),
    }),
  removeMember: (name: string, user: string) =>
    request(`/api/teams/${encodeURIComponent(name)}/members/${encodeURIComponent(user)}`, {
      method: "DELETE",
    }),
  archiveTeam: (name: string) =>
    request(`/api/teams/${encodeURIComponent(name)}/archive`, { method: "POST", body: "{}" }),
  leaveTeam: (name: string) =>
    request(`/api/teams/${encodeURIComponent(name)}/leave`, { method: "POST", body: "{}" }),
  users: () => request<{ users: User[] }>("/api/users"),
  createUser: (username: string, role: string) =>
    request<{ user: User; password: string }>("/api/users", {
      method: "POST",
      body: JSON.stringify({ username, role }),
    }),
  updateUser: (name: string, status: string) =>
    request(`/api/users/${encodeURIComponent(name)}`, {
      method: "PATCH",
      body: JSON.stringify({ status }),
    }),
  resetPassword: (name: string) =>
    request<{ password: string }>(`/api/users/${encodeURIComponent(name)}/password/reset`, {
      method: "POST",
      body: "{}",
    }),
  issueToken: (name: string) =>
    request<{ token: string }>(`/api/users/${encodeURIComponent(name)}/tokens`, {
      method: "POST",
      body: "{}",
    }),
  audit: (limit = 200) => request<{ audit: AuditEntry[] }>(`/api/audit?limit=${limit}`),
  allSessions: () => request<{ sessions: SessionView[] }>("/api/sessions"),
};

export function fmtTime(unix?: number): string {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString("zh-CN");
}
