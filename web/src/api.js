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
  borrow: (payload) => req('POST', '/borrows', payload),
  returnById: (borrowId) => req('POST', '/returns', { borrow_id: borrowId }),
  returnByCredential: (credential) => req('POST', '/returns', { credential }),
  adjustQuota: (payload) => req('POST', '/quotas', payload),
  reclaim: () => req('POST', '/admin/reclaim'),
}
