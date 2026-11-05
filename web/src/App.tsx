import { useCallback, useEffect, useState } from "react";
import {
  api,
  ApiError,
  fmtTime,
  type AuditEntry,
  type Member,
  type SessionView,
  type Team,
  type TokenInfo,
  type User,
} from "./api";

function errText(e: unknown): string {
  if (e instanceof ApiError) return e.message;
  return e instanceof Error ? e.message : String(e);
}

export default function App() {
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    try {
      const me = await api.me();
      setUser(me.user);
      setError("");
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) setUser(null);
      else setError(errText(e));
    }
  }, []);

  useEffect(() => {
    refresh().finally(() => setLoading(false));
  }, [refresh]);

  if (loading) return <div className="center muted">加载中…</div>;
  if (error) return <div className="center error">{error}</div>;
  if (!user)
    return (
      <Login
        onLogin={(u) => {
          setUser(u);
          if (u.must_change_password) refresh();
        }}
      />
    );
  if (user.must_change_password)
    return (
      <ChangePassword
        title="首次登录，请修改口令"
        onSubmit={async (oldPw, newPw) => {
          await api.changePassword(oldPw, newPw);
          await refresh();
        }}
      />
    );

  return (
    <Console
      user={user}
      onLogout={async () => {
        await api.logout();
        setUser(null);
      }}
    />
  );
}

function Login({ onLogin }: { onLogin: (u: User) => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <div className="center">
      <form
        className="card auth"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            const r = await api.login(username, password);
            onLogin(r.user);
          } catch (err) {
            setError(errText(err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <h1>AgentWire</h1>
        <p className="muted">团队 Claude 会话通信管理后台</p>
        <label>
          用户名
          <input value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
        </label>
        <label>
          口令
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        {error && <p className="error">{error}</p>}
        <button disabled={busy}>{busy ? "登录中…" : "登录"}</button>
      </form>
    </div>
  );
}

function ChangePassword({
  title,
  onSubmit,
}: {
  title: string;
  onSubmit: (oldPw: string, newPw: string) => Promise<void>;
}) {
  const [oldPw, setOldPw] = useState("");
  const [newPw, setNewPw] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <div className="center">
      <form
        className="card auth"
        onSubmit={async (e) => {
          e.preventDefault();
          if (newPw !== confirm) return setError("两次输入不一致");
          if (newPw.length < 8) return setError("新口令至少 8 位");
          setBusy(true);
          setError("");
          try {
            await onSubmit(oldPw, newPw);
          } catch (err) {
            setError(errText(err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <h1>修改口令</h1>
        <p className="muted">{title}</p>
        <label>
          原口令
          <input type="password" value={oldPw} onChange={(e) => setOldPw(e.target.value)} autoFocus />
        </label>
        <label>
          新口令
          <input type="password" value={newPw} onChange={(e) => setNewPw(e.target.value)} />
        </label>
        <label>
          再次输入
          <input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
        </label>
        {error && <p className="error">{error}</p>}
        <button disabled={busy}>{busy ? "提交中…" : "提交"}</button>
      </form>
    </div>
  );
}

type Tab = "teams" | "sessions" | "tokens" | "users" | "audit";

function Console({ user, onLogout }: { user: User; onLogout: () => void }) {
  const [tab, setTab] = useState<Tab>("teams");
  return (
    <div className="layout">
      <aside>
        <h2>AgentWire</h2>
        <p className="muted small">
          {user.username} · {user.role === "admin" ? "平台管理员" : "成员"}
        </p>
        <nav>
          <button className={tab === "teams" ? "active" : ""} onClick={() => setTab("teams")}>
            团队
          </button>
          <button
            className={tab === "sessions" ? "active" : ""}
            onClick={() => setTab("sessions")}
          >
            在线会话
          </button>
          <button className={tab === "tokens" ? "active" : ""} onClick={() => setTab("tokens")}>
            我的 Token
          </button>
          {user.role === "admin" && (
            <>
              <button className={tab === "users" ? "active" : ""} onClick={() => setTab("users")}>
                用户管理
              </button>
              <button className={tab === "audit" ? "active" : ""} onClick={() => setTab("audit")}>
                审计
              </button>
            </>
          )}
        </nav>
        <button className="ghost logout" onClick={onLogout}>
          退出登录
        </button>
      </aside>
      <main>
        {tab === "teams" && <Teams />}
        {tab === "sessions" && <Sessions admin={user.role === "admin"} />}
        {tab === "tokens" && <Tokens />}
        {tab === "users" && user.role === "admin" && <Users />}
        {tab === "audit" && user.role === "admin" && <Audit />}
      </main>
    </div>
  );
}

function useAsync<T>(fn: () => Promise<T>, deps: unknown[]) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  const reload = useCallback(() => {
    fn()
      .then((d) => {
        setData(d);
        setError("");
      })
      .catch((e) => setError(errText(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  useEffect(reload, [reload]);
  return { data, error, reload, setData };
}

function Teams() {
  const { data, error, reload } = useAsync(() => api.teams(), []);
  const [name, setName] = useState("");
  const [code, setCode] = useState("");
  const [selected, setSelected] = useState<string | null>(null);
  const [msg, setMsg] = useState("");

  return (
    <section>
      <h1>团队</h1>
      {error && <p className="error">{error}</p>}
      <div className="row">
        <form
          className="inline"
          onSubmit={async (e) => {
            e.preventDefault();
            if (!name.trim()) return;
            try {
              await api.createTeam(name.trim());
              setName("");
              setMsg("团队已创建");
              reload();
            } catch (err) {
              setMsg(errText(err));
            }
          }}
        >
          <input placeholder="新 team 名称" value={name} onChange={(e) => setName(e.target.value)} />
          <button>创建 team</button>
        </form>
        <form
          className="inline"
          onSubmit={async (e) => {
            e.preventDefault();
            if (!code.trim()) return;
            try {
              await api.joinTeam(code.trim());
              setCode("");
              setMsg("已加入 team");
              reload();
            } catch (err) {
              setMsg(errText(err));
            }
          }}
        >
          <input placeholder="邀请码" value={code} onChange={(e) => setCode(e.target.value)} />
          <button>加入</button>
        </form>
      </div>
      {msg && <p className="notice">{msg}</p>}
      <div className="grid">
        {(data?.teams ?? []).map((t) => (
          <button
            key={t.id}
            className={"tile " + (selected === t.name ? "selected" : "")}
            onClick={() => setSelected(t.name)}
          >
            <strong>{t.name}</strong>
            <span className="muted small">
              {t.role === "owner" ? "owner" : "member"} · {t.members} 人
            </span>
          </button>
        ))}
        {data && (data.teams?.length ?? 0) === 0 && <p className="muted">还没有加入任何 team。</p>}
      </div>
      {selected && <TeamDetail name={selected} onChanged={reload} />}
    </section>
  );
}

function TeamDetail({ name, onChanged }: { name: string; onChanged: () => void }) {
  const { data, error, reload } = useAsync(() => api.team(name), [name]);
  const [ttl, setTtl] = useState(168);
  const [maxUses, setMaxUses] = useState(1);
  const [invite, setInvite] = useState("");
  const [msg, setMsg] = useState("");

  if (error) return <p className="error">{error}</p>;
  if (!data) return <p className="muted">加载中…</p>;

  return (
    <div className="card detail">
      <h2>
        {data.team.name} <span className="muted small">({data.team.role})</span>
      </h2>
      {data.stats && (
        <div className="stats">
          <span>发送 {data.stats.sent}</span>
          <span>送达 {data.stats.delivered}</span>
          <span>排队 {data.stats.queued}</span>
          <span>失败 {data.stats.failed}</span>
        </div>
      )}
      <h3>成员</h3>
      <table>
        <thead>
          <tr>
            <th>用户名</th>
            <th>角色</th>
            <th>加入时间</th>
            {data.team.owner && <th></th>}
          </tr>
        </thead>
        <tbody>
          {(data.members ?? []).map((m: Member) => (
            <tr key={m.id}>
              <td>{m.username}</td>
              <td>{m.role}</td>
              <td>{fmtTime(m.joined_at)}</td>
              {data.team.owner && (
                <td>
                  {m.role !== "owner" && (
                    <button
                      className="danger small"
                      onClick={async () => {
                        if (!confirm(`移除 ${m.username}？`)) return;
                        try {
                          await api.removeMember(name, m.username);
                          reload();
                          onChanged();
                        } catch (e) {
                          setMsg(errText(e));
                        }
                      }}
                    >
                      移除
                    </button>
                  )}
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>

      {data.team.owner ? (
        <>
          <h3>邀请码</h3>
          <div className="inline">
            <label className="compact">
              有效小时
              <input
                type="number"
                value={ttl}
                min={1}
                onChange={(e) => setTtl(Number(e.target.value))}
              />
            </label>
            <label className="compact">
              使用上限
              <input
                type="number"
                value={maxUses}
                min={1}
                onChange={(e) => setMaxUses(Number(e.target.value))}
              />
            </label>
            <button
              onClick={async () => {
                try {
                  const r = await api.createInvite(name, ttl, maxUses);
                  setInvite(r.code);
                  reload();
                } catch (e) {
                  setMsg(errText(e));
                }
              }}
            >
              生成邀请码
            </button>
          </div>
          {invite && (
            <p className="notice">
              一次性展示：<code>{invite}</code>
            </p>
          )}
          {data.invites && data.invites.length > 0 && (
            <ul className="muted small">
              {data.invites.map((i, idx) => (
                <li key={idx}>
                  到期 {fmtTime(i.expires_at)} · 已用 {i.used_count}/{i.max_uses}
                </li>
              ))}
            </ul>
          )}
          <button
            className="danger"
            onClick={async () => {
              if (!confirm(`归档 team ${name}？归档后成员将不可见。`)) return;
              try {
                await api.archiveTeam(name);
                onChanged();
              } catch (e) {
                setMsg(errText(e));
              }
            }}
          >
            归档 team
          </button>
        </>
      ) : (
        <button
          className="danger"
          onClick={async () => {
            if (!confirm(`退出 team ${name}？`)) return;
            try {
              await api.leaveTeam(name);
              onChanged();
            } catch (e) {
              setMsg(errText(e));
            }
          }}
        >
          退出 team
        </button>
      )}
      {msg && <p className="notice">{msg}</p>}
    </div>
  );
}

function Sessions({ admin }: { admin: boolean }) {
  const [all, setAll] = useState(false);
  const { data, error, reload } = useAsync(
    () => (admin && all ? api.allSessions() : api.sessions()),
    [admin, all]
  );
  return (
    <section>
      <h1>在线会话</h1>
      <div className="row">
        <button className="ghost" onClick={reload}>
          刷新
        </button>
        {admin && (
          <label className="inline small">
            <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />
            显示全部用户
          </label>
        )}
      </div>
      {error && <p className="error">{error}</p>}
      <SessionTable sessions={data?.sessions ?? []} showUser />
    </section>
  );
}

function SessionTable({ sessions, showUser }: { sessions: SessionView[]; showUser?: boolean }) {
  if (sessions.length === 0) return <p className="muted">当前没有在线会话。</p>;
  return (
    <table>
      <thead>
        <tr>
          {showUser && <th>用户</th>}
          <th>机器</th>
          <th>会话</th>
          <th>项目</th>
          <th>状态</th>
          <th>模式</th>
        </tr>
      </thead>
      <tbody>
        {sessions.map((s, i) => (
          <tr key={i}>
            {showUser && <td>{s.user}</td>}
            <td>{s.machine}</td>
            <td>
              {s.session.name}
              <div className="muted tiny">{s.session.cwd}</div>
            </td>
            <td>{s.session.project || "—"}</td>
            <td>
              <span className={"badge " + (s.session.status === "idle" ? "ok" : "busy")}>
                {s.session.status}
              </span>
            </td>
            <td>{s.session.mode}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function Tokens() {
  const { data, error, reload } = useAsync(() => api.tokens(), []);
  const [fresh, setFresh] = useState("");
  return (
    <section>
      <h1>我的 Token</h1>
      <p className="muted small">用于 agentwire daemon 接入中心服务。token 只在创建时展示一次。</p>
      {error && <p className="error">{error}</p>}
      <button
        onClick={async () => {
          try {
            const r = await api.createToken();
            setFresh(r.token);
            reload();
          } catch (e) {
            alert(errText(e));
          }
        }}
      >
        签发新 token
      </button>
      {fresh && (
        <p className="notice">
          请立即保存：<code>{fresh}</code>
        </p>
      )}
      <TokenTable tokens={data?.tokens ?? []} onRevoke={reload} />
    </section>
  );
}

function TokenTable({ tokens, onRevoke }: { tokens: TokenInfo[]; onRevoke: () => void }) {
  if (tokens.length === 0) return <p className="muted">还没有 token。</p>;
  return (
    <table>
      <thead>
        <tr>
          <th>ID</th>
          <th>创建时间</th>
          <th>最后使用</th>
          <th>状态</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {tokens.map((t) => (
          <tr key={t.id}>
            <td>{t.id}</td>
            <td>{fmtTime(t.created_at)}</td>
            <td>{fmtTime(t.last_used_at)}</td>
            <td>{t.revoked_at ? "已吊销" : "有效"}</td>
            <td>
              {!t.revoked_at && (
                <button
                  className="danger small"
                  onClick={async () => {
                    if (!confirm("吊销这个 token？")) return;
                    try {
                      await api.revokeToken(t.id);
                      onRevoke();
                    } catch (e) {
                      alert(errText(e));
                    }
                  }}
                >
                  吊销
                </button>
              )}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function Users() {
  const { data, error, reload } = useAsync(() => api.users(), []);
  const [username, setUsername] = useState("");
  const [role, setRole] = useState("member");
  const [secret, setSecret] = useState("");
  return (
    <section>
      <h1>用户管理</h1>
      {error && <p className="error">{error}</p>}
      <form
        className="inline"
        onSubmit={async (e) => {
          e.preventDefault();
          try {
            const r = await api.createUser(username, role);
            setSecret(`用户 ${r.user.username} 的初始口令：${r.password}`);
            setUsername("");
            reload();
          } catch (err) {
            alert(errText(err));
          }
        }}
      >
        <input placeholder="用户名" value={username} onChange={(e) => setUsername(e.target.value)} />
        <select value={role} onChange={(e) => setRole(e.target.value)}>
          <option value="member">member</option>
          <option value="admin">admin</option>
        </select>
        <button>创建用户</button>
      </form>
      {secret && <p className="notice">{secret}</p>}
      <table>
        <thead>
          <tr>
            <th>用户名</th>
            <th>角色</th>
            <th>状态</th>
            <th>创建时间</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {(data?.users ?? []).map((u) => (
            <tr key={u.id}>
              <td>{u.username}</td>
              <td>{u.role}</td>
              <td>{u.status === "active" ? "启用" : "禁用"}</td>
              <td>{fmtTime(u.created_at)}</td>
              <td className="actions">
                <button
                  className="ghost small"
                  onClick={async () => {
                    try {
                      await api.updateUser(u.username, u.status === "active" ? "disabled" : "active");
                      reload();
                    } catch (e) {
                      alert(errText(e));
                    }
                  }}
                >
                  {u.status === "active" ? "禁用" : "启用"}
                </button>
                <button
                  className="ghost small"
                  onClick={async () => {
                    if (!confirm(`重置 ${u.username} 的口令？`)) return;
                    try {
                      const r = await api.resetPassword(u.username);
                      setSecret(`${u.username} 的新口令：${r.password}`);
                      reload();
                    } catch (e) {
                      alert(errText(e));
                    }
                  }}
                >
                  重置口令
                </button>
                <button
                  className="ghost small"
                  onClick={async () => {
                    try {
                      const r = await api.issueToken(u.username);
                      setSecret(`${u.username} 的新 token：${r.token}`);
                    } catch (e) {
                      alert(errText(e));
                    }
                  }}
                >
                  签发 token
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function Audit() {
  const { data, error } = useAsync(() => api.audit(300), []);
  return (
    <section>
      <h1>审计记录</h1>
      <p className="muted small">审计只保存元数据，不保存消息正文。</p>
      {error && <p className="error">{error}</p>}
      <table>
        <thead>
          <tr>
            <th>时间</th>
            <th>操作者</th>
            <th>动作</th>
            <th>team</th>
            <th>msg_id</th>
            <th>大小</th>
            <th>结果</th>
            <th>来源 IP</th>
          </tr>
        </thead>
        <tbody>
          {(data?.audit ?? []).map((a: AuditEntry) => (
            <tr key={a.id}>
              <td>{fmtTime(a.at)}</td>
              <td>{a.actor_user || "—"}</td>
              <td>{a.action}</td>
              <td>{a.team_id || "—"}</td>
              <td className="mono">{a.msg_id || "—"}</td>
              <td>{a.size}</td>
              <td>{a.result}</td>
              <td>{a.source_ip}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
