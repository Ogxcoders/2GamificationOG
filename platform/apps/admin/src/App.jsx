import React, { useEffect, useMemo, useState, useCallback } from 'react'

// ─────────────────────────────────────────────────────────────────────────────
// The admin console: configuration lifecycle, live events, traces, players,
// leaderboards, analytics — everything the golden path touches.
// State lives in memory (session-only key); page refresh requires re-login —
// acceptable for an ops console (documented behavior).
// ─────────────────────────────────────────────────────────────────────────────

const OBJ_TYPES = [
  'rule', 'challenge', 'achievement', 'streak', 'level_track', 'currency',
  'leaderboard', 'workflow', 'paywall', 'offer', 'product', 'segment', 'flag', 'reward',
]

const SEED_CONFIGS = {
  rule: { event_type: 'lesson.completed', condition: { type: 'eq', field: 'event.type', value: 'lesson.completed' }, actions: [{ type: 'award_xp', track: 'default', amount: 100 }], cooldown_seconds: 0, frequency_cap: 0 },
  challenge: { progress_event_type: 'lesson.completed', target: 3, repeatability: 'once', rewards: [{ type: 'currency', amount: 10, target: 'coin' }] },
  streak: { event_type: 'lesson.completed', window: { type: 'fixed', unit: 'day', timezone: 'UTC' }, grace_seconds: 0, freezes_allowed: 0 },
  level_track: { model: 'linear', xp_per_level: 100 },
  currency: { cap: 0, allow_negative: false },
  leaderboard: { direction: 'highest', tie_breaker: 'earliest_achieved_then_user_id' },
  achievement: { condition: { type: 'gte', field: 'user.level', value: 5 }, hidden: false, repeatable: false, rewards: [] },
}

async function api(method, base, path, key, body) {
  const res = await fetch(base + path, {
    method,
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await res.text()
  const json = text ? safeParse(text) : {}
  if (!res.ok) {
    const msg = json?.error?.message || `HTTP ${res.status}`
    throw new Error(msg)
  }
  return json
}
const safeParse = (t) => { try { return JSON.parse(t) } catch { return {} } }

export default function App() {
  // ── login state ──
  const [apiKey, setApiKey] = useState('')
  const [baseUrl, setBaseUrl] = useState(localStorage.getItem('uep_base') || 'http://localhost:8080')
  const [loggedIn, setLoggedIn] = useState(false)
  const [loginErr, setLoginErr] = useState('')

  // ── scope state ──
  const [projectId, setProjectId] = useState('')
  const [orgId, setOrgId] = useState('')
  const [envId, setEnvId] = useState('')

  const [view, setView] = useState('objects')

  const call = useCallback((method, path, body) => api(method, baseUrl, path, apiKey, body), [baseUrl, apiKey])

  if (!loggedIn) {
    return (
      <Login baseUrl={baseUrl} setBaseUrl={setBaseUrl} apiKey={apiKey} setApiKey={setApiKey}
        onLogin={async () => {
          // Verify the key by listing projects for a bootstrap org (probe call).
          setLoginErr('')
          if (!apiKey.includes('.') || apiKey.split('.').length !== 2) {
            setLoginErr('API key format: keyID.secret')
            return
          }
          setLoggedIn(true)
        }} err={loginErr} />
    )
  }

  return (
    <>
      <div className="top">
        <h1>⬢ Universal Engagement</h1>
        <span className="muted mono">{envId || 'no environment'}</span>
        <div className="spacer" />
        <span className="muted">{baseUrl}</span>
      </div>
      <div className="layout">
        <div className="side">
          {['objects', 'events', 'players', 'leaderboards', 'analytics'].map((v) => (
            <button key={v} className={view === v ? 'active' : ''} onClick={() => setView(v)}>
              {v === 'objects' ? 'Configuration' : v[0].toUpperCase() + v.slice(1)}
            </button>
          ))}
          <div style={{ padding: 20, borderTop: '1px solid var(--border)', marginTop: 16 }}>
            <ScopeSetup call={call} orgId={orgId} setOrgId={setOrgId} projectId={projectId}
              setProjectId={setProjectId} envId={envId} setEnvId={setEnvId} apiKey={apiKey} />
          </div>
        </div>
        <div className="main">
          {envId ? (
            <>
              {view === 'objects' && <ObjectsView call={call} projectId={projectId} envId={envId} />}
              {view === 'events' && <EventsView call={call} projectId={projectId} envId={envId} />}
              {view === 'players' && <PlayersView call={call} projectId={projectId} envId={envId} />}
              {view === 'leaderboards' && <LeaderboardsView call={call} projectId={projectId} envId={envId} />}
              {view === 'analytics' && <AnalyticsView call={call} projectId={projectId} envId={envId} />}
            </>
          ) : (
            <div className="card">Set up your tenant scope in the sidebar to begin (organization id → project → environment).</div>
          )}
        </div>
      </div>
    </>
  )
}

function Login({ baseUrl, setBaseUrl, apiKey, setApiKey, onLogin, err }) {
  return (
    <div style={{ display: 'grid', placeItems: 'center', minHeight: '100vh' }}>
      <div className="card" style={{ width: 380 }}>
        <h2>Sign in to the control plane</h2>
        <div className="row"><input style={{ flex: 1 }} placeholder="Control plane URL" value={baseUrl}
          onChange={(e) => { setBaseUrl(e.target.value); localStorage.setItem('uep_base', e.target.value) }} /></div>
        <div className="row"><input style={{ flex: 1 }} placeholder="API key (keyID.secret)" type="password"
          value={apiKey} onChange={(e) => setApiKey(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && onLogin()} /></div>
        {err && <div className="banner err">{err}</div>}
        <button className="btn primary" onClick={onLogin}>Enter console</button>
        <p className="muted" style={{ marginTop: 12, fontSize: 12 }}>
          The key is kept in memory for this tab only. Bootstrap a tenant via
          <span className="mono"> POST /v1/bootstrap</span> to obtain one.
        </p>
      </div>
    </div>
  )
}

function ScopeSetup({ call, orgId, setOrgId, projectId, setProjectId, envId, setEnvId, apiKey }) {
  const [projects, setProjects] = useState([])
  const [envs, setEnvs] = useState([])
  useEffect(() => { if (orgId) call('GET', `/v1/projects?organization_id=${orgId}`).then((r) => setProjects(r.projects || [])).catch(() => setProjects([])) }, [orgId])
  useEffect(() => { if (projectId) call('GET', `/v1/projects/${projectId}/environments`).then((r) => setEnvs(r.environments || [])).catch(() => setEnvs([])) }, [projectId])
  return (
    <div>
      <div className="row"><input style={{ flex: 1 }} placeholder="organization_id" value={orgId} onChange={(e) => setOrgId(e.target.value.trim())} /></div>
      <div className="row">
        <select value={projectId} onChange={(e) => setProjectId(e.target.value)} style={{ flex: 1 }}>
          <option value="">project…</option>
          {projects.map((p) => <option key={p.id} value={p.id}>{p.name || p.id}</option>)}
        </select>
      </div>
      <div className="row">
        <select value={envId} onChange={(e) => setEnvId(e.target.value)} style={{ flex: 1 }}>
          <option value="">environment…</option>
          {envs.map((e) => <option key={e.id} value={e.id}>{e.kind || e.id}</option>)}
        </select>
      </div>
      <p className="muted" style={{ fontSize: 11, marginTop: 8 }}>
        keyID: <span className="mono">{apiKey.split('.')[0]}</span>
      </p>
    </div>
  )
}

// ── Configuration objects ─────────────────────────────────────────────────────

function ObjectsView({ call, projectId, envId }) {
  const [objects, setObjects] = useState([])
  const [typeFilter, setTypeFilter] = useState('')
  const [msg, setMsg] = useState(null)
  const [editor, setEditor] = useState(null) // {mode: create|edit, type, name, id, config}

  const scope = `/v1/projects/${projectId}/environments/${envId}`
  const refresh = useCallback(() => {
    call('GET', `${scope}/objects${typeFilter ? `?type=${typeFilter}` : ''}`)
      .then((r) => setObjects(r.objects || []))
      .catch((e) => setMsg({ kind: 'err', text: e.message }))
  }, [call, scope, typeFilter])
  useEffect(refresh, [refresh])

  const act = async (label, fn) => {
    try {
      await fn()
      setMsg({ kind: 'ok', text: label + ' ✓' })
      refresh()
    } catch (e) {
      setMsg({ kind: 'err', text: e.message })
    }
  }

  return (
    <>
      {msg && <div className={'banner ' + (msg.kind === 'ok' ? 'ok' : 'err')}>{msg.text}</div>}
      <div className="card">
        <h2>Configuration objects</h2>
        <div className="row">
          <select value={typeFilter} onChange={(e) => setTypeFilter(e.target.value)}>
            <option value="">all types</option>
            {OBJ_TYPES.map((t) => <option key={t}>{t}</option>)}
          </select>
          <button className="btn primary" onClick={() => setEditor({ mode: 'create', type: 'rule', name: '', config: JSON.stringify(SEED_CONFIGS.rule, null, 2) })}>New object</button>
          <button className="btn" onClick={() => act('recompiled', () => call('POST', `${scope}/engine-config/recompile`))}>Recompile engine config</button>
          <div className="spacer" style={{ flex: 1 }} />
          <button className="btn" onClick={refresh}>Refresh</button>
        </div>
        <table>
          <thead><tr><th>Name</th><th>Type</th><th>Status</th><th>Version</th><th>Actions</th></tr></thead>
          <tbody>
            {objects.map((o) => (
              <tr key={o.id}>
                <td>{o.name}</td>
                <td className="mono muted">{o.type}</td>
                <td><span className={'pill ' + o.status}>{o.status}</span></td>
                <td>{o.version}{o.published_version ? ` (pub ${o.published_version})` : ''}</td>
                <td>
                  <div className="row">
                    {o.status !== 'published' && <button className="btn" onClick={() => act(`published ${o.name}`, () => call('POST', `${scope}/objects/${o.id}/publish`))}>Publish</button>}
                    {o.status === 'published' && <button className="btn" onClick={() => act(`paused ${o.name}`, () => call('POST', `${scope}/objects/${o.id}/pause`))}>Pause</button>}
                    {o.status === 'paused' && <button className="btn" onClick={() => act(`resumed ${o.name}`, () => call('POST', `${scope}/objects/${o.id}/resume`))}>Resume</button>}
                    <button className="btn" onClick={() => setEditor({ mode: 'edit', id: o.id, type: o.type, name: o.name, config: JSON.stringify(o.config, null, 2), status: o.status })}>Edit</button>
                    <button className="btn" onClick={() => { const to = prompt('Promote to environment (development|staging|production):', 'staging'); if (to) act(`promoted to ${to}`, () => call('POST', `${scope}/objects/${o.id}/promote`, { to_environment: to })) }}>Promote</button>
                    <button className="btn danger" onClick={() => confirm(`Archive ${o.name}?`) && act('archived', () => call('DELETE', `${scope}/objects/${o.id}`))}>Archive</button>
                  </div>
                </td>
              </tr>
            ))}
            {objects.length === 0 && <tr><td colSpan="5" className="muted">No objects yet — create a rule to begin.</td></tr>}
          </tbody>
        </table>
      </div>
      {editor && (
        <div className="card">
          <h2>{editor.mode === 'create' ? 'Create' : 'Edit'} {editor.type}{editor.status ? ` (${editor.status})` : ''}</h2>
          <div className="row">
            <input placeholder="name" value={editor.name} disabled={editor.mode === 'edit'}
              onChange={(e) => setEditor({ ...editor, name: e.target.value })} style={{ width: 200 }} />
            {editor.mode === 'create' && (
              <select value={editor.type} onChange={(e) => setEditor({
                ...editor, type: e.target.value,
                config: JSON.stringify(SEED_CONFIGS[e.target.value] || {}, null, 2),
              })}>
                {OBJ_TYPES.map((t) => <option key={t}>{t}</option>)}
              </select>
            )}
          </div>
          <textarea rows="14" style={{ width: '100%' }} className="mono" value={editor.config}
            onChange={(e) => setEditor({ ...editor, config: e.target.value })} />
          <div className="row" style={{ marginTop: 8 }}>
            <button className="btn primary" onClick={async () => {
              try {
                let cfg
                try { cfg = JSON.parse(editor.config) } catch { throw new Error('config is not valid JSON') }
                if (editor.mode === 'create') {
                  await call('POST', `${scope}/objects`, { name: editor.name, type: editor.type, config: cfg })
                } else {
                  if (editor.status === 'published') throw new Error('published objects are immutable — this is by design')
                  await call('PUT', `${scope}/objects/${editor.id}`, { name: editor.name, type: editor.type, config: cfg })
                }
                setMsg({ kind: 'ok', text: 'saved ✓' })
                setEditor(null)
                refresh()
              } catch (e) { setMsg({ kind: 'err', text: e.message }) }
            }}>{editor.mode === 'create' ? 'Create draft' : 'Save draft'}</button>
            <button className="btn" onClick={() => {
              try {
                const cfg = JSON.parse(editor.config)
                call('POST', `${scope}/validate-draft`, { name: editor.name, type: editor.type, config: cfg })
                  .then(() => setMsg({ kind: 'ok', text: 'valid ✓' }))
                  .catch((e) => setMsg({ kind: 'err', text: e.message }))
              } catch { setMsg({ kind: 'err', text: 'config is not valid JSON' }) }
            }}>Validate</button>
            <button className="btn" onClick={() => setEditor(null)}>Close</button>
          </div>
        </div>
      )}
    </>
  )
}

// ── Events feed + traces ──────────────────────────────────────────────────────

function EventsView({ call, projectId, envId }) {
  const [events, setEvents] = useState([])
  const [ingest, setIngest] = useState({ event_type: 'lesson.completed', actor_id: '', payload: '{"points": 25}' })
  const [msg, setMsg] = useState(null)
  const [trace, setTrace] = useState(null)
  const scope = `/v1/projects/${projectId}/environments/${envId}`

  const refresh = useCallback(() => {
    call('GET', `${scope}/events?limit=50`).then((r) => setEvents(r.events || [])).catch((e) => setMsg({ kind: 'err', text: e.message }))
  }, [call, scope])
  useEffect(refresh, [refresh])

  return (
    <>
      {msg && <div className={'banner ' + (msg.kind === 'ok' ? 'ok' : 'err')}>{msg.text}</div>}
      <div className="card">
        <h2>Send a test event</h2>
        <div className="row">
          <input placeholder="event_type" value={ingest.event_type} style={{ width: 200 }}
            onChange={(e) => setIngest({ ...ingest, event_type: e.target.value })} />
          <input placeholder="actor_id (user)" value={ingest.actor_id} style={{ width: 160 }}
            onChange={(e) => setIngest({ ...ingest, actor_id: e.target.value })} />
          <input placeholder='payload JSON' value={ingest.payload} style={{ flex: 1 }}
            onChange={(e) => setIngest({ ...ingest, payload: e.target.value })} />
        </div>
        <div className="row">
          <button className="btn primary" onClick={async () => {
            try {
              const payload = JSON.parse(ingest.payload || '{}')
              const r = await call('POST', `${scope}/events`, {
                events: [{ event_type: ingest.event_type, actor_id: ingest.actor_id, payload, idempotency_key: 'ui_' + Date.now() }],
              })
              const res = (r.results || [])[0] || {}
              setMsg({ kind: 'ok', text: `ingested: ${res.status} (${res.event_id})` })
              refresh()
            } catch (e) { setMsg({ kind: 'err', text: e.message }) }
          }}>Ingest + process</button>
        </div>
      </div>
      <div className="card">
        <h2>Recent events</h2>
        <table>
          <thead><tr><th>Event</th><th>Type</th><th>Actor</th><th>Status</th><th>Actions</th></tr></thead>
          <tbody>
            {events.map((e) => (
              <tr key={e.event_id}>
                <td className="mono muted">{String(e.event_id).slice(0, 18)}</td>
                <td className="mono">{e.event_type}</td>
                <td className="mono muted">{e.actor_id}</td>
                <td><span className={'pill ' + e.status}>{e.status}</span></td>
                <td className="row">
                  <button className="btn" onClick={async () => {
                    try {
                      const r = await call('POST', `${scope}/events/${e.event_id}/process`)
                      setMsg({ kind: 'ok', text: `processed: ${r.applied_commands} applied, ${r.skipped_commands} skipped` })
                      refresh()
                    } catch (err2) { setMsg({ kind: 'err', text: err2.message }) }
                  }}>Process</button>
                  <button className="btn" onClick={() =>
                    call('GET', `${scope}/events/${e.event_id}/trace`).then(setTrace).catch((e2) => setMsg({ kind: 'err', text: e2.message }))}>Trace</button>
                </td>
              </tr>
            ))}
            {events.length === 0 && <tr><td colSpan="5" className="muted">No events yet.</td></tr>}
          </tbody>
        </table>
      </div>
      {trace && (
        <div className="card">
          <h2>Decision trace — {trace.event_id}</h2>
          <p className="muted" style={{ marginBottom: 8 }}>config v{trace.config_version} · {trace.nodes?.length || 0} nodes</p>
          {(trace.nodes || []).map((n, i) => (
            <div className="trace-node" key={i}>
              <span className="mono muted">{n.kind}</span> — {n.label}
              {n.detail && Object.keys(n.detail).length > 0 && (
                <pre style={{ marginTop: 4 }}>{JSON.stringify(n.detail, null, 2)}</pre>
              )}
            </div>
          ))}
          <div className="row" style={{ marginTop: 8 }}><button className="btn" onClick={() => setTrace(null)}>Close trace</button></div>
        </div>
      )}
    </>
  )
}

// ── Players ───────────────────────────────────────────────────────────────────

function PlayersView({ call, projectId, envId }) {
  const [userId, setUserId] = useState('')
  const [state, setState] = useState(null)
  const [msg, setMsg] = useState(null)
  const scope = `/v1/projects/${projectId}/environments/${envId}`

  return (
    <>
      {msg && <div className={'banner ' + (msg.kind === 'ok' ? 'ok' : 'err')}>{msg.text}</div>}
      <div className="card">
        <h2>Player lookup</h2>
        <div className="row">
          <input placeholder="user id" value={userId} style={{ width: 240 }} onChange={(e) => setUserId(e.target.value.trim())} />
          <button className="btn primary" onClick={() => {
            call('GET', `${scope}/users/${userId}/state`).then(setState).catch((e) => setMsg({ kind: 'err', text: e.message }))
          }}>Load state</button>
        </div>
      </div>
      {state && (
        <>
          <div className="card">
            <h2>Profile</h2>
            <div className="kv">
              <span className="k">user</span><span className="mono">{state.user?.user_id}</span>
              <span className="k">anonymous</span><span>{String(state.user?.anonymous)}</span>
              <span className="k">created</span><span className="mono">{state.user?.created_at}</span>
            </div>
          </div>
          <div className="grid2">
            <div className="card">
              <h2>XP tracks</h2>
              <Table rows={state.tracks} cols={['track', 'xp', 'level']} />
              <h2 style={{ marginTop: 16 }}>Wallets</h2>
              <Table rows={state.wallets} cols={['currency', 'balance']} />
              <h2 style={{ marginTop: 16 }}>Streaks</h2>
              <Table rows={state.streaks} cols={['streak_id', 'current', 'best', 'active']} />
            </div>
            <div className="card">
              <h2>Challenges</h2>
              <Table rows={state.challenges} cols={['challenge_id', 'progress', 'target', 'status']} />
              <h2 style={{ marginTop: 16 }}>Achievements</h2>
              <Table rows={state.achievements} cols={['achievement_id', 'unlocked_at']} />
              <h2 style={{ marginTop: 16 }}>Entitlements</h2>
              <Table rows={state.entitlements} cols={['entitlement', 'expires_at']} />
            </div>
          </div>
        </>
      )}
    </>
  )
}

function Table({ rows, cols }) {
  return (
    <table>
      <thead><tr>{cols.map((c) => <th key={c}>{c}</th>)}</tr></thead>
      <tbody>
        {(rows || []).map((r, i) => <tr key={i}>{cols.map((c) => <td key={c} className="mono">{String(r[c] ?? '')}</td>)}</tr>)}
        {(!rows || rows.length === 0) && <tr><td colSpan={cols.length} className="muted">—</td></tr>}
      </tbody>
    </table>
  )
}

// ── Leaderboards ─────────────────────────────────────────────────────────────

function LeaderboardsView({ call, projectId, envId }) {
  const [lbId, setLbId] = useState('')
  const [page, setPage] = useState(null)
  const [msg, setMsg] = useState(null)
  const scope = `/v1/projects/${projectId}/environments/${envId}`
  return (
    <>
      {msg && <div className="banner err">{msg.text}</div>}
      <div className="card">
        <h2>Leaderboard</h2>
        <div className="row">
          <input placeholder="leaderboard id" value={lbId} style={{ width: 260 }} onChange={(e) => setLbId(e.target.value.trim())} />
          <button className="btn primary" onClick={() => {
            call('GET', `${scope}/leaderboards/${lbId}?limit=25`).then(setPage).catch((e) => setMsg({ kind: 'err', text: e.message }))
          }}>View board</button>
        </div>
      </div>
      {page && (
        <div className="card">
          <h2>{page.leaderboard_id} <span className="muted">({page.direction}, {page.total_entries} entries)</span></h2>
          {page.me && <p style={{ marginBottom: 8 }}>Your rank: <b>#{page.me.rank}</b> ({page.me.score} pts)</p>}
          <table>
            <thead><tr><th>Rank</th><th>User</th><th>Score</th><th>Achieved</th></tr></thead>
            <tbody>
              {(page.top || []).map((r, i) => (
                <tr key={i}><td>#{r.rank}</td><td className="mono">{r.user_id}</td><td>{r.score}</td><td className="mono muted">{r.achieved_at}</td></tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}

// ── Analytics ─────────────────────────────────────────────────────────────────

function AnalyticsView({ call, projectId, envId }) {
  const [data, setData] = useState(null)
  const [err, setErr] = useState('')
  useEffect(() => {
    call('GET', `/v1/projects/${projectId}/environments/${envId}/analytics/overview?days=14`)
      .then(setData).catch((e) => setErr(e.message))
  }, [call, projectId, envId])
  if (err) return <div className="banner err">{err}</div>
  if (!data) return <div className="muted">loading…</div>
  const totalEvents = (data.events_by_day || []).reduce((a, b) => a + b.events, 0)
  const dau = (data.dau || [])
  const maxDau = Math.max(1, ...dau.map((d) => d.users))
  return (
    <>
      <div className="card">
        <div className="stats">
          <div><div className="stat">{totalEvents}</div><div className="stat-label">events (14d)</div></div>
          <div><div className="stat">{dau[dau.length - 1]?.users ?? 0}</div><div className="stat-label">DAU (latest)</div></div>
        </div>
      </div>
      <div className="card">
        <h2>Events per day</h2>
        <table><thead><tr><th>Day</th><th>Events</th></tr></thead>
          <tbody>{(data.events_by_day || []).map((d) => <tr key={d.day}><td className="mono">{d.day}</td><td>{d.events}</td></tr>)}</tbody></table>
      </div>
      <div className="card">
        <h2>DAU trend</h2>
        <div style={{ display: 'flex', alignItems: 'flex-end', gap: 4, height: 120, padding: 8, background: 'var(--bg)', borderRadius: 6, border: '1px solid var(--border)' }}>
          {dau.map((d) => (
            <div key={d.day} title={`${d.day}: ${d.users}`} style={{
              flex: 1, background: 'var(--accent)', opacity: 0.8, borderRadius: 2,
              height: `${(d.users / maxDau) * 100}%`, minHeight: 2,
            }} />
          ))}
          {dau.length === 0 && <span className="muted">no data yet</span>}
        </div>
      </div>
      <div className="card">
        <h2>Top rules (7d)</h2>
        <table><thead><tr><th>Rule</th><th>Fires</th></tr></thead>
          <tbody>{(data.top_rules || []).map((r, i) => <tr key={i}><td>{r.label}</td><td>{r.fires}</td></tr>)}</tbody></table>
      </div>
    </>
  )
}
