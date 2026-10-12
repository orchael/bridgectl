// API client — all URLs are relative so they work in both dev and production.

export interface Session {
  sessionId: string
  projectId: string
  provider: string
  status: string
  createdAt: string
  repoPath: string
}

export interface SessionEvent {
  type: 'output' | 'geometry' | 'ready' | 'error' | 'end'
  cols?: number
  rows?: number
  data?: string     // base64 for output
  message?: string  // for error
}

export interface StartParams {
  remote?: string
  project: string
  provider: string
  repoPath: string
}

export interface RemoteEntry {
  name: string
  host: string
}

export async function listRemotes(): Promise<RemoteEntry[]> {
  const res = await fetch('/api/remotes')
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error ?? `HTTP ${res.status}`)
  }
  const data = await res.json()
  return data.remotes ?? []
}

function remoteQuery(remote: string | undefined): string {
  return remote ? `?remote=${encodeURIComponent(remote)}` : ''
}

export async function listSessions(remote?: string): Promise<Session[]> {
  const res = await fetch(`/api/sessions${remoteQuery(remote)}`)
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error ?? `HTTP ${res.status}`)
  }
  const data = await res.json()
  return data.sessions ?? []
}

export async function startSession(params: StartParams): Promise<string> {
  const res = await fetch('/api/sessions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      remote: params.remote,
      project: params.project,
      provider: params.provider,
      repoPath: params.repoPath,
    }),
  })
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error ?? `HTTP ${res.status}`)
  }
  const data = await res.json()
  return data.sessionId
}

export async function stopSession(id: string, remote?: string): Promise<void> {
  const res = await fetch(`/api/sessions/${encodeURIComponent(id)}${remoteQuery(remote)}`, {
    method: 'DELETE',
  })
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error ?? `HTTP ${res.status}`)
  }
}

export function streamSession(
  id: string,
  role: 'observer' | 'writer',
  remote: string | undefined,
  clientId: string,
  onEvent: (e: SessionEvent) => void,
  signal: AbortSignal
): void {
  const params = new URLSearchParams({ role, clientId })
  if (remote) params.set('remote', remote)
  const url = `/api/sessions/${encodeURIComponent(id)}/stream?${params}`

  const es = new EventSource(url)
  signal.addEventListener('abort', () => es.close())

  es.onmessage = (e) => {
    try {
      const event = JSON.parse(e.data) as SessionEvent
      onEvent(event)
      if (event.type === 'end') {
        es.close()
      }
    } catch {
      // ignore parse errors
    }
  }

  es.onerror = () => {
    onEvent({ type: 'end' })
    es.close()
  }
}

export async function writeInput(
  id: string,
  clientId: string,
  data: Uint8Array,
  remote?: string
): Promise<void> {
  // encode to base64
  const b64 = btoa(String.fromCharCode(...data))
  const res = await fetch(`/api/sessions/${encodeURIComponent(id)}/input`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ data: b64, clientId, remote }),
  })
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error ?? `HTTP ${res.status}`)
  }
}

export async function resizeSession(
  id: string,
  clientId: string,
  cols: number,
  rows: number,
  remote?: string
): Promise<void> {
  const res = await fetch(`/api/sessions/${encodeURIComponent(id)}/resize`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ cols, rows, clientId, remote }),
  })
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error ?? `HTTP ${res.status}`)
  }
}
