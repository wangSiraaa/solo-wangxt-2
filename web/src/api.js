const BASE = '/api'

async function req(method, path, body) {
  const res = await fetch(BASE + path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  })
  const data = await res.json().catch(() => ({}))
  return { status: res.status, data, cache: res.headers.get('X-Cache') }
}

export const api = {
  overview: () => req('GET', '/overview'),
  authority: () => req('GET', '/authority'),
  peerAuthority: (base) =>
    fetch(`${base}/api/authority`).then((r) => (r.ok ? r.json() : null)).catch(() => null),
  borrow: (payload) => req('POST', '/borrows', payload),
  migrate: (borrowId, departmentId) =>
    req('POST', `/borrows/${borrowId}/migrate`, { department_id: departmentId }),
  returnById: (borrowId) => req('POST', '/returns', { borrow_id: borrowId }),
  returnByCredential: (credential) => req('POST', '/returns', { credential }),
  adjustQuota: (payload) => req('POST', '/quotas', payload),
  reclaim: () => req('POST', '/admin/reclaim'),
  takeover: (reason) => req('POST', '/admin/takeover', { reason }),
}
