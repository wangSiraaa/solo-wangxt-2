import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from './api.js'

// 本浏览器借出的离线凭证,供"提前归还"演示使用(真实场景由员工客户端持有)
const CRED_STORE_KEY = 'licensehub:credentials'
const loadCreds = () => JSON.parse(localStorage.getItem(CRED_STORE_KEY) || '{}')
const saveCred = (borrowId, credential) => {
  const all = loadCreds()
  all[borrowId] = credential
  localStorage.setItem(CRED_STORE_KEY, JSON.stringify(all))
}
const dropCred = (borrowId) => {
  const all = loadCreds()
  delete all[borrowId]
  localStorage.setItem(CRED_STORE_KEY, JSON.stringify(all))
}

// 对端实例地址(双实例演示:8080 <-> 8081)
const peerBase = () => {
  const { hostname, port } = window.location
  return `http://${hostname}:${port === '8081' ? '8080' : '8081'}`
}

function fmtLeft(seconds) {
  if (seconds == null) return '—'
  if (seconds < 0) return '已过期,待回收'
  const m = Math.floor(seconds / 60)
  const s = seconds % 60
  return m > 0 ? `${m}分${s}秒` : `${s}秒`
}

function fmtTime(iso) {
  if (!iso) return '—'
  return new Date(iso).toLocaleTimeString()
}

function QuotaBar({ used, quota }) {
  const pct = quota > 0 ? Math.min(100, (used / quota) * 100) : used > 0 ? 100 : 0
  const over = used > quota
  return (
    <div className="quota-bar" title={`占用 ${used} / 额度 ${quota}`}>
      <div className={`quota-fill ${over ? 'over' : ''}`} style={{ width: `${pct}%` }} />
      <span className="quota-text">
        {used}/{quota}
        {over && ' 超额,仅限制新申请'}
      </span>
    </div>
  )
}

function PoolCard({ pool }) {
  return (
    <div className="card pool-card">
      <div className="pool-head">
        <h3>{pool.product}</h3>
        <span className="seats">
          {pool.active}<small>/{pool.total_seats} 席</small>
        </span>
      </div>
      <div className="pool-modes">
        <span className="tag online">在线 {pool.online}</span>
        <span className="tag offline">离线 {pool.offline}</span>
      </div>
      <table className="dept-table">
        <thead>
          <tr><th>部门</th><th>额度占用</th><th>在线</th><th>离线</th></tr>
        </thead>
        <tbody>
          {pool.departments.map((d) => (
            <tr key={d.department_id}>
              <td>{d.name}</td>
              <td><QuotaBar used={d.active} quota={d.quota} /></td>
              <td>{d.online}</td>
              <td>{d.offline}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// 实例权威状态:本实例 + 对端实例 + 数据库租约(唯一事实)+ 接管历史
function AuthorityPanel({ auth, peerAuth, onTakeover }) {
  if (!auth) return null
  const lease = auth.lease
  const leaseLeft = Math.max(0, Math.round((new Date(lease.lease_expires_at) - Date.now()) / 1000))
  const instances = [
    { label: '本实例', data: auth },
    peerAuth ? { label: '对端实例', data: peerAuth } : null,
  ].filter(Boolean)

  return (
    <div className="card authority-card">
      <h3>实例权威状态</h3>
      <table className="dept-table">
        <thead>
          <tr><th></th><th>实例</th><th>权威?</th><th>本地认知</th></tr>
        </thead>
        <tbody>
          {instances.map(({ label, data }) => (
            <tr key={label}>
              <td className="muted">{label}</td>
              <td>{data.instance_id}</td>
              <td>
                <span className={`tag ${data.is_authoritative ? 'online' : 'offline'}`}>
                  {data.is_authoritative ? '权威(可签发)' : '备用(围栏拦截)'}
                </span>
              </td>
              <td className="muted">
                held={String(data.local_view.held)} epoch={data.local_view.epoch}
              </td>
            </tr>
          ))}
          {!peerAuth && (
            <tr><td className="muted">对端实例</td><td colSpan="3" className="muted">不可达(单实例或对端已停止)</td></tr>
          )}
        </tbody>
      </table>
      <div className="lease-line">
        数据库租约(唯一事实):持有者 <b>{lease.holder_id || '—'}</b> · 代次 <b>{lease.epoch}</b> ·{' '}
        {lease.valid ? `剩余 ${leaseLeft}s` : '已过期'} · 接管原因:{lease.takeover_reason || '—'}
        <button className="mini" style={{ marginLeft: 10 }} onClick={onTakeover}>手动接管</button>
      </div>
      {auth.history?.length > 0 && (
        <details className="history">
          <summary>接管历史({auth.history.length})</summary>
          <ul>
            {auth.history.map((h, i) => (
              <li key={i}>[{fmtTime(h.at)}] epoch {h.epoch} → {h.holder_id} · {h.reason}</li>
            ))}
          </ul>
        </details>
      )}
    </div>
  )
}

function BorrowPanel({ pools, departments, onDone, log }) {
  const [poolId, setPoolId] = useState('')
  const [deptId, setDeptId] = useState('')
  const [employee, setEmployee] = useState('张三')
  const [mode, setMode] = useState('online')
  const [ttl, setTtl] = useState(120)
  const [idemKey, setIdemKey] = useState(() => crypto.randomUUID())
  const [result, setResult] = useState(null)
  const [busy, setBusy] = useState(false)

  const submit = async (e) => {
    e?.preventDefault()
    setBusy(true)
    const r = await api.borrow({
      pool_id: Number(poolId),
      department_id: Number(deptId),
      employee,
      mode,
      ttl_seconds: mode === 'offline' ? Number(ttl) : 0,
      idempotency_key: idemKey,
    })
    setBusy(false)
    setResult(r)
    if (r.status === 201 || r.status === 200) {
      if (r.data.credential) saveCred(r.data.borrow.id, r.data.credential)
      log(`${r.status === 201 ? `借用成功(代次 ${r.data.borrow.issued_epoch})` : '幂等重放(网络重试返回原结果)'} · #${r.data.borrow.id} ${employee} ${mode}`, r.status === 201 ? 'ok' : 'info')
      onDone()
    } else {
      log(`借用被拒 · ${r.data.error}: ${r.data.message}`, 'err')
    }
  }

  return (
    <div className="card">
      <h3>借用席位</h3>
      <form onSubmit={submit} className="form">
        <label>许可证池
          <select value={poolId} onChange={(e) => setPoolId(e.target.value)} required>
            <option value="">选择…</option>
            {pools.map((p) => <option key={p.id} value={p.id}>{p.product}</option>)}
          </select>
        </label>
        <label>部门
          <select value={deptId} onChange={(e) => setDeptId(e.target.value)} required>
            <option value="">选择…</option>
            {departments.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}
          </select>
        </label>
        <label>员工
          <input value={employee} onChange={(e) => setEmployee(e.target.value)} required />
        </label>
        <label>模式
          <select value={mode} onChange={(e) => setMode(e.target.value)}>
            <option value="online">在线(连接占用)</option>
            <option value="offline">离线(凭证借出)</option>
          </select>
        </label>
        {mode === 'offline' && (
          <label>离线时长(秒)
            <input type="number" min="1" value={ttl} onChange={(e) => setTtl(e.target.value)} />
          </label>
        )}
        <label>幂等键
          <div className="idem-row">
            <code>{idemKey.slice(0, 13)}…</code>
            <button type="button" className="mini" onClick={() => setIdemKey(crypto.randomUUID())}>换</button>
          </div>
        </label>
        <button disabled={busy}>{busy ? '提交中…' : '借用'}</button>
        <button
          type="button"
          className="secondary"
          disabled={busy}
          onClick={async () => { await submit(); await submit() }}
        >
          借用并重试(同键)
        </button>
      </form>
      {result && (
        <div className={`result ${result.status < 300 ? 'ok' : 'err'}`}>
          <div>HTTP {result.status} {result.data.replay ? '· replay=true' : ''}</div>
          {result.data.error && <div>{result.data.error}: {result.data.message}</div>}
          {result.data.credential && (
            <div className="cred-box">
              <div>离线归还凭证(代次 {result.data.borrow.issued_epoch},已存本地):</div>
              <code>{result.data.credential}</code>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function MigrateCell({ borrow, departments, onDone, log }) {
  const [target, setTarget] = useState('')
  const doMigrate = async () => {
    if (!target) return
    const r = await api.migrate(borrow.id, Number(target))
    if (r.status === 200) {
      log(`迁移 · #${borrow.id} → 部门${target}(池占用不变)`, 'ok')
    } else {
      log(`迁移被拒 · #${borrow.id} ${r.data.error}`, 'err')
    }
    onDone()
  }
  return (
    <span className="migrate-cell">
      <select value={target} onChange={(e) => setTarget(e.target.value)}>
        <option value="">迁移到…</option>
        {departments.filter((d) => d.id !== borrow.department_id).map((d) => (
          <option key={d.id} value={d.id}>{d.name}</option>
        ))}
      </select>
      <button className="mini" onClick={doMigrate} disabled={!target}>迁移</button>
    </span>
  )
}

function ActiveBorrows({ borrows, departments, onDone, log }) {
  const [manualCred, setManualCred] = useState('')
  const doReturn = async (b) => {
    let r
    if (b.mode === 'online') {
      r = await api.returnById(b.id)
    } else {
      const cred = loadCreds()[b.id] || manualCred
      if (!cred) {
        alert('此离线借用不是从本浏览器借出,请在下方粘贴归还凭证')
        return
      }
      r = await api.returnByCredential(cred)
    }
    if (r.status === 200) {
      dropCred(b.id)
      log(`归还 ${r.data.result === 'returned' ? '成功' : '幂等:已关闭'} · #${b.id}`, 'ok')
    } else {
      log(`归还被拒 · #${b.id} ${r.data.error}: ${r.data.message}`, 'err')
    }
    onDone()
  }

  return (
    <div className="card wide">
      <h3>当前占用({borrows.length})</h3>
      {borrows.length === 0 ? <p className="muted">暂无占用</p> : (
        <table className="borrow-table">
          <thead>
            <tr><th>ID</th><th>池</th><th>部门</th><th>员工</th><th>模式</th><th>代次</th><th>离线剩余</th><th></th><th></th></tr>
          </thead>
          <tbody>
            {borrows.map((b) => (
              <tr key={b.id}>
                <td>#{b.id}</td>
                <td>{b.product}</td>
                <td>{b.department}</td>
                <td>{b.employee}</td>
                <td><span className={`tag ${b.mode}`}>{b.mode === 'online' ? '在线' : '离线'}</span></td>
                <td className="muted">{b.issued_epoch}</td>
                <td className={b.seconds_left != null && b.seconds_left < 30 ? 'warn' : ''}>
                  {b.mode === 'offline' ? fmtLeft(b.seconds_left) : '—'}
                </td>
                <td><button className="mini" onClick={() => doReturn(b)}>归还</button></td>
                <td><MigrateCell borrow={b} departments={departments} onDone={onDone} log={log} /></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <details className="manual-cred">
        <summary>手动粘贴离线归还凭证(含旧代次凭证)</summary>
        <textarea
          rows="2"
          placeholder="v1.xxx.yyy"
          value={manualCred}
          onChange={(e) => setManualCred(e.target.value)}
        />
      </details>
    </div>
  )
}

function QuotaPanel({ pools, departments, onDone, log }) {
  const [poolId, setPoolId] = useState('')
  const [deptId, setDeptId] = useState('')
  const [quota, setQuota] = useState(1)
  const [by, setBy] = useState('部门负责人-王工')
  const [result, setResult] = useState(null)

  const submit = async (e) => {
    e.preventDefault()
    const r = await api.adjustQuota({
      pool_id: Number(poolId), department_id: Number(deptId),
      quota: Number(quota), adjusted_by: by,
    })
    setResult(r)
    if (r.status === 200) {
      log(`额度调整 · 池${poolId} 部门${deptId} → ${quota}(当前占用 ${r.data.active_borrows})`, 'ok')
      onDone()
    } else {
      log(`额度调整失败 · ${r.data.error}`, 'err')
    }
  }

  return (
    <div className="card">
      <h3>部门额度调整</h3>
      <form onSubmit={submit} className="form">
        <label>许可证池
          <select value={poolId} onChange={(e) => setPoolId(e.target.value)} required>
            <option value="">选择…</option>
            {pools.map((p) => <option key={p.id} value={p.id}>{p.product}</option>)}
          </select>
        </label>
        <label>部门
          <select value={deptId} onChange={(e) => setDeptId(e.target.value)} required>
            <option value="">选择…</option>
            {departments.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}
          </select>
        </label>
        <label>新额度
          <input type="number" min="0" value={quota} onChange={(e) => setQuota(e.target.value)} />
        </label>
        <label>调整人
          <input value={by} onChange={(e) => setBy(e.target.value)} />
        </label>
        <button>调整</button>
      </form>
      {result && (
        <div className={`result ${result.status === 200 ? 'ok' : 'err'}`}>
          <div>HTTP {result.status}</div>
          {result.data.note && <div>{result.data.note}</div>}
          {result.data.error && <div>{result.data.error}: {result.data.message}</div>}
        </div>
      )}
    </div>
  )
}

export default function App() {
  const [ov, setOv] = useState(null)
  const [auth, setAuth] = useState(null)
  const [peerAuth, setPeerAuth] = useState(null)
  const [cacheState, setCacheState] = useState('')
  const [logs, setLogs] = useState([])
  const [, forceTick] = useState(0)
  const logRef = useRef(0)

  const log = useCallback((msg, level = 'info') => {
    setLogs((prev) => [{ id: ++logRef.current, time: new Date().toLocaleTimeString(), msg, level }, ...prev].slice(0, 20))
  }, [])

  const refresh = useCallback(async () => {
    const [ovr, aur, peer] = await Promise.all([
      api.overview(), api.authority(), api.peerAuthority(peerBase()),
    ])
    if (ovr.status === 200) {
      setOv(ovr.data)
      setCacheState(ovr.cache || 'OFF')
    }
    if (aur.status === 200) setAuth(aur.data)
    setPeerAuth(peer)
  }, [])

  useEffect(() => {
    refresh()
    const t = setInterval(refresh, 3000)
    return () => clearInterval(t)
  }, [refresh])

  // 每秒重绘一次,让倒计时动起来
  useEffect(() => {
    const t = setInterval(() => forceTick((n) => n + 1), 1000)
    return () => clearInterval(t)
  }, [])

  const doReclaim = async () => {
    const r = await api.reclaim()
    log(`过期回收 · 回收 ${r.data.reclaimed} 个席位`, 'ok')
    refresh()
  }

  const doTakeover = async () => {
    const r = await api.takeover('管理员在控制台手动接管')
    if (r.data.acquired) {
      log(`手动接管成功 · epoch ${r.data.lease.epoch}`, 'ok')
    } else {
      log(`接管未生效 · 当前持有者 ${r.data.lease.holder_id}(租约未过期)`, 'info')
    }
    refresh()
  }

  if (!ov) return <div className="loading">加载中…</div>

  return (
    <div className="page">
      <header>
        <h1>LicenseHub · 浮动许可证管理
          {auth && <span className="instance-badge">{auth.instance_id}</span>}
        </h1>
        <div className="header-right">
          <span className={`cache-badge ${cacheState.toLowerCase()}`}>查询缓存 {cacheState}</span>
          <button className="mini" onClick={doReclaim}>立即过期回收</button>
        </div>
      </header>

      {auth && <AuthorityPanel auth={auth} peerAuth={peerAuth} onTakeover={doTakeover} />}

      <section className="pools">
        {ov.pools.map((p) => <PoolCard key={p.id} pool={p} />)}
      </section>

      <section className="grid">
        <BorrowPanel pools={ov.pools} departments={ov.departments} onDone={refresh} log={log} />
        <ActiveBorrows borrows={ov.active_borrows} departments={ov.departments} onDone={refresh} log={log} />
        <QuotaPanel pools={ov.pools} departments={ov.departments} onDone={refresh} log={log} />
        <div className="card">
          <h3>事件日志</h3>
          <ul className="log">
            {logs.length === 0 && <li className="muted">操作后在此显示</li>}
            {logs.map((l) => <li key={l.id} className={l.level}>[{l.time}] {l.msg}</li>)}
          </ul>
        </div>
      </section>
    </div>
  )
}
