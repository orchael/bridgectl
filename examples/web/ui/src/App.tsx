import { useState, useCallback, useEffect } from 'react'
import SessionList from './components/SessionList'
import StartSessionForm from './components/StartSessionForm'
import Terminal from './components/Terminal'
import OrchestratorExample from './components/OrchestratorExample'
import { listRemotes, type RemoteEntry } from './api'

type ConnectionMode = 'local' | 'remote'

interface ActiveSession {
  id: string
  role: 'observer' | 'writer'
}

const styles: Record<string, React.CSSProperties> = {
  app: {
    height: '100vh',
    display: 'flex',
    flexDirection: 'column' as const,
    overflow: 'hidden',
    background: '#1a1a2e',
    color: '#e0e0e0',
    fontFamily: 'system-ui, -apple-system, sans-serif',
  },
  header: {
    background: '#16213e',
    borderBottom: '1px solid #0f3460',
    padding: '16px 24px',
    display: 'flex',
    alignItems: 'center',
    gap: '24px',
    flexWrap: 'wrap' as const,
  },
  title: {
    fontSize: '20px',
    fontWeight: 700,
    color: '#e94560',
    margin: 0,
  },
  connectionBar: {
    display: 'flex',
    alignItems: 'center',
    gap: '12px',
    flexWrap: 'wrap' as const,
  },
  label: {
    fontSize: '14px',
    color: '#a0a0b0',
  },
  radioGroup: {
    display: 'flex',
    gap: '12px',
  },
  radioLabel: {
    display: 'flex',
    alignItems: 'center',
    gap: '4px',
    cursor: 'pointer',
    fontSize: '14px',
  },
  input: {
    background: '#0f3460',
    border: '1px solid #e94560',
    borderRadius: '4px',
    color: '#e0e0e0',
    padding: '4px 10px',
    fontSize: '14px',
    outline: 'none',
    width: '220px',
  },
  select: {
    background: '#0f3460',
    border: '1px solid #e94560',
    borderRadius: '4px',
    color: '#e0e0e0',
    padding: '4px 10px',
    fontSize: '14px',
    outline: 'none',
    width: '240px',
  },
  refreshBtn: {
    background: '#0f3460',
    border: '1px solid #1a4a8a',
    borderRadius: '4px',
    color: '#e0e0e0',
    padding: '4px 8px',
    fontSize: '14px',
    cursor: 'pointer',
    lineHeight: 1,
  },
  main: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column' as const,
    padding: '24px',
    overflow: 'hidden',
    minHeight: 0,
  },
  terminalPanel: {
    flex: 1,
    minHeight: 0,
    display: 'flex',
    flexDirection: 'column' as const,
    marginTop: '24px',
    border: '1px solid #0f3460',
    borderRadius: '8px',
    overflow: 'hidden',
  },
}

export default function App() {
  const [mode, setMode] = useState<ConnectionMode>('local')
  const [view, setView] = useState<'sessions' | 'orchestrator'>('sessions')
  const [remoteHost, setRemoteHost] = useState('')
  const [customRemote, setCustomRemote] = useState(false)
  const [activeSession, setActiveSession] = useState<ActiveSession | null>(null)
  const [showNewSession, setShowNewSession] = useState(false)
  const [refreshKey, setRefreshKey] = useState(0)
  const [knownRemotes, setKnownRemotes] = useState<RemoteEntry[]>([])
  const [remotesLoading, setRemotesLoading] = useState(false)

  const refreshRemotes = useCallback(async () => {
    setRemotesLoading(true)
    try {
      const remotes = await listRemotes()
      setKnownRemotes(remotes)
    } catch {
      // ignore
    } finally {
      setRemotesLoading(false)
    }
  }, [])

  useEffect(() => {
    void refreshRemotes()
  }, [refreshRemotes])

  const remote = mode === 'remote' && remoteHost ? remoteHost : undefined

  function handleAttach(id: string) {
    setActiveSession({ id, role: 'writer' })
  }

  function handleWatch(id: string) {
    setActiveSession({ id, role: 'observer' })
  }

  function handleCloseTerminal() {
    setActiveSession(null)
  }

  const handleSessionStarted = useCallback((sessionId: string) => {
    setShowNewSession(false)
    setRefreshKey((k) => k + 1)
    setActiveSession({ id: sessionId, role: 'writer' })
  }, [])

  return (
    <div style={styles.app}>
      <header style={styles.header}>
        <h1 style={styles.title}>AI Agent Bridge</h1>
        <div style={styles.connectionBar}>
          <span style={styles.label}>Connect to:</span>
          <div style={styles.radioGroup}>
            <label style={styles.radioLabel}>
              <input
                type="radio"
                name="mode"
                value="local"
                checked={mode === 'local'}
                onChange={() => setMode('local')}
              />
              Local
            </label>
            <label style={styles.radioLabel}>
              <input
                type="radio"
                name="mode"
                value="remote"
                checked={mode === 'remote'}
                onChange={() => setMode('remote')}
              />
              Remote
            </label>
          </div>
          {mode === 'remote' && (
            <>
              {knownRemotes.length > 0 && !customRemote ? (
                <>
                  <select
                    style={styles.select}
                    value={remoteHost}
                    onChange={(e) => {
                      if (e.target.value === '__custom__') {
                        setCustomRemote(true)
                        setRemoteHost('')
                      } else {
                        setRemoteHost(e.target.value)
                      }
                    }}
                  >
                    <option value="">Select a server...</option>
                    {knownRemotes.map((r) => (
                      <option key={r.host} value={r.host}>
                        {r.name} ({r.host})
                      </option>
                    ))}
                    <option value="__custom__">Other...</option>
                  </select>
                  <button
                    type="button"
                    style={{
                      ...styles.refreshBtn,
                      ...(remotesLoading ? { cursor: 'not-allowed', opacity: 0.6 } : {}),
                    }}
                    onClick={() => void refreshRemotes()}
                    disabled={remotesLoading}
                    aria-label="Refresh remotes from config"
                    title="Refresh remotes from config"
                  >
                    {remotesLoading ? '...' : '\u21BB'}
                  </button>
                </>
              ) : (
                <>
                  <input
                    style={styles.input}
                    type="text"
                    placeholder="hostname or host:port"
                    value={remoteHost}
                    onChange={(e) => setRemoteHost(e.target.value)}
                  />
                  <button
                    type="button"
                    style={{
                      ...styles.refreshBtn,
                      ...(remotesLoading ? { cursor: 'not-allowed', opacity: 0.6 } : {}),
                    }}
                    onClick={() => void refreshRemotes()}
                    disabled={remotesLoading}
                    aria-label="Refresh remotes from config"
                    title="Refresh remotes from config"
                  >
                    {remotesLoading ? '...' : '\u21BB'}
                  </button>
                  {knownRemotes.length > 0 && (
                    <button
                      style={{ ...styles.input, width: 'auto', cursor: 'pointer' }}
                      onClick={() => {
                        setCustomRemote(false)
                        setRemoteHost('')
                      }}
                    >
                      Back
                    </button>
                  )}
                </>
              )}
            </>
          )}
        </div>
      </header>

      <nav style={{ padding: '10px 24px', display: 'flex', gap: 12 }}><button onClick={() => setView('sessions')}>Sessions</button><button onClick={() => setView('orchestrator')}>Orchestrator example</button></nav>
      <main style={{ ...styles.main, overflow: 'auto' }}>
        {view === 'orchestrator' ? <OrchestratorExample remote={remote} /> : <>
        <SessionList
          key={refreshKey}
          remote={remote}
          onAttach={handleAttach}
          onWatch={handleWatch}
          onNewSession={() => setShowNewSession(true)}
        />

        {activeSession && (
          <div style={styles.terminalPanel}>
            <Terminal
              sessionId={activeSession.id}
              role={activeSession.role}
              remote={remote}
              onClose={handleCloseTerminal}
              onSwitchToWatch={() =>
                setActiveSession({ id: activeSession.id, role: 'observer' })
              }
            />
          </div>
        )}
        </>}
      </main>

      {showNewSession && (
        <StartSessionForm
          remote={remote}
          onStarted={handleSessionStarted}
          onCancel={() => setShowNewSession(false)}
        />
      )}
    </div>
  )
}
