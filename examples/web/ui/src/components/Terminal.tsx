import { useEffect, useRef, useState } from 'react'
import { Terminal as XTerm } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { streamSession, writeInput, resizeSession } from '../api'

interface Props {
  sessionId: string
  role: 'observer' | 'writer'
  remote: string | undefined
  onClose: () => void
  onSwitchToWatch?: () => void
}

const styles: Record<string, React.CSSProperties> = {
  wrapper: {
    display: 'flex',
    flexDirection: 'column' as const,
    flex: 1,
    minHeight: 0,
    background: '#000',
  },
  toolbar: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    background: '#16213e',
    borderBottom: '1px solid #0f3460',
    padding: '8px 16px',
    flexShrink: 0,
  },
  info: {
    fontSize: '13px',
    color: '#a0a0b0',
    fontFamily: 'monospace',
  },
  closeBtn: {
    background: 'transparent',
    border: '1px solid #e94560',
    borderRadius: '4px',
    color: '#e94560',
    padding: '4px 12px',
    fontSize: '13px',
    cursor: 'pointer',
  },
  termContainer: {
    flex: 1,
    overflow: 'auto',
    minHeight: 0,
    padding: '4px',
  },
  conflictOverlay: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'center',
    justifyContent: 'center',
    background: '#0d0d0d',
    color: '#e0e0e0',
  },
  watchBtn: {
    background: '#0f3460',
    border: '1px solid #1a4a8a',
    borderRadius: '4px',
    color: '#e0e0e0',
    padding: '6px 16px',
    fontSize: '13px',
    cursor: 'pointer',
  },
}

export default function Terminal({ sessionId, role, remote, onClose, onSwitchToWatch }: Props) {
  const containerRef = useRef<HTMLDivElement>(null)
  const xtermRef = useRef<XTerm | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const clientId = useRef<string>(crypto.randomUUID())
  const abortRef = useRef<AbortController | null>(null)
  const [writerConflict, setWriterConflict] = useState(false)

  useEffect(() => {
    if (!containerRef.current) return

    const term = new XTerm({
      theme: {
        background: '#0d0d0d',
        foreground: '#e0e0e0',
        cursor: '#e94560',
      },
      fontFamily: 'monospace',
      fontSize: 14,
      cursorBlink: true,
      disableStdin: true,
      scrollback: 100000,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(containerRef.current)
    fit.fit()
    xtermRef.current = term
    fitRef.current = fit

    const abort = new AbortController()
    abortRef.current = abort
    let attached = false
    let sendResize = () => {}
    let rendering = Promise.resolve()
    let inputQueue = Promise.resolve()
    let pendingInputBytes = 0

    // Stream output from server via SSE
    streamSession(
      sessionId,
      role,
      remote,
      clientId.current,
      (event) => {
        rendering = rendering.then(async () => {
          if (abort.signal.aborted) return
          // Preserve the PTY grid even when the viewer's browser is narrower.
          // Output frames carry dimensions at capture time, including resizes.
          if (event.cols && event.rows && event.cols <= 500 && event.rows <= 300 &&
              (term.cols !== event.cols || term.rows !== event.rows)) {
            term.resize(event.cols, event.rows)
          }
          if (event.type === 'output' && event.data) {
            const bytes = Uint8Array.from(atob(event.data), (c) => c.charCodeAt(0))
            await new Promise<void>(resolve => term.write(bytes, resolve))
          } else if (event.type === 'ready') {
            attached = true
            term.options.disableStdin = role !== 'writer'
            if (role === 'writer') { fit.fit(); sendResize() }
          } else if (event.type === 'error' && event.message) {
            if (event.message.includes('already has an active writer')) {
              setWriterConflict(true)
            } else {
              term.writeln(`\r\n\x1b[31m[error] ${event.message}\x1b[0m`)
            }
          } else if (event.type === 'end') {
            attached = false
            term.options.disableStdin = true
            term.writeln('\r\n\x1b[33m[session ended]\x1b[0m')
          }
        }).catch(error => { if (!abort.signal.aborted) console.error('terminal render failed:', error) })
      },
      abort.signal
    )

    // For writer role: send input
    let inputDispose: { dispose(): void } | undefined
    if (role === 'writer') {
      inputDispose = term.onData((data) => {
        if (!attached) return
        const bytes = new TextEncoder().encode(data)
        if (pendingInputBytes + bytes.length > 65536) {
          attached = false
          term.options.disableStdin = true
          term.writeln('\r\n[input queue full; detach and attach again]')
          return
        }
        pendingInputBytes += bytes.length
        inputQueue = inputQueue.then(async () => {
          if (abort.signal.aborted || !attached) return
          for (let offset = 0; offset < bytes.length; offset += 4096) {
            await writeInput(sessionId, clientId.current, bytes.subarray(offset, offset + 4096), remote)
          }
        }).catch((err) => {
          attached = false
          term.options.disableStdin = true
          console.error('writeInput failed:', err)
        }).finally(() => { pendingInputBytes -= bytes.length })
      })

      // Send initial resize
      sendResize = () => {
        if (!attached || abort.signal.aborted) return
        void resizeSession(
          sessionId,
          clientId.current,
          term.cols,
          term.rows,
          remote
        ).catch((err) => {
          console.error('resizeSession failed:', err)
        })
      }

      // Resize observer
      const observer = new ResizeObserver(() => {
        if (!attached) return
        fit.fit()
        sendResize()
      })
      if (containerRef.current) {
        observer.observe(containerRef.current)
      }
      sendResize()

      return () => {
        abort.abort()
        inputDispose?.dispose()
        observer.disconnect()
        term.dispose()
      }
    }

    return () => {
      abort.abort()
      term.dispose()
    }
  }, [sessionId, role, remote])

  const shortId = sessionId.length > 12 ? sessionId.slice(0, 8) + '…' : sessionId

  return (
    <div style={styles.wrapper}>
      <div style={styles.toolbar}>
        <span style={styles.info}>
          Session: <strong>{shortId}</strong> &nbsp;|&nbsp; Mode:{' '}
          <strong>{role}</strong>
          {remote && (
            <>
              &nbsp;|&nbsp; Remote: <strong>{remote}</strong>
            </>
          )}
        </span>
        <button style={styles.closeBtn} onClick={onClose}>
          Detach
        </button>
      </div>
      {writerConflict ? (
        <div style={styles.conflictOverlay}>
          <p style={{ margin: '0 0 12px', fontSize: '15px' }}>
            This session already has an active writer attached.
          </p>
          <div style={{ display: 'flex', gap: '10px' }}>
            {onSwitchToWatch && (
              <button style={styles.watchBtn} onClick={onSwitchToWatch}>
                Watch instead
              </button>
            )}
            <button style={styles.closeBtn} onClick={onClose}>
              Detach
            </button>
          </div>
        </div>
      ) : (
        <div ref={containerRef} style={styles.termContainer} />
      )}
    </div>
  )
}
