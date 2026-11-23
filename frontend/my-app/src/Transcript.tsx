import { useLayoutEffect, useRef, type ReactElement } from 'react'
import type { Citation, ThoughtSource } from './api'
import './Transcript.css'

export type DisplayMessage = {
  key: string
  role: string
  text: string
  citations?: Citation[]
  source?: ThoughtSource
  navigation?: boolean
  inherited?: boolean
}

interface Props {
  messages: DisplayMessage[]
  pendingQuestion?: string
  loading: boolean
  error?: string
  onRetry: () => void
}

function MessageText({ text }: { text: string }) {
  const blocks: ReactElement[] = []
  let lines: string[] = []
  let fenceLength = 0
  function flush() {
    const value = lines.join('\n')
    if (fenceLength) {
      blocks.push(<pre key={blocks.length}><code>{value}</code></pre>)
    } else {
      for (const paragraph of value.split(/\n[ \t]*\n/)) {
        if (paragraph.trim()) blocks.push(<p key={blocks.length}>{paragraph}</p>)
      }
    }
    lines = []
  }
  for (const line of text.replace(/\r\n?/g, '\n').split('\n')) {
    const fence = line.match(/^ {0,3}(`{3,})([^`]*)$/)
    if (fence && (!fenceLength || (fence[1].length >= fenceLength && !fence[2].trim()))) {
      flush()
      fenceLength = fenceLength ? 0 : fence[1].length
    } else {
      lines.push(line)
    }
  }
  flush()
  return <div className="transcript-text">{blocks}</div>
}

function location(source: ThoughtSource) {
  const range = source.seq_start === source.seq_end ? source.seq_start : `${source.seq_start}–${source.seq_end}`
  return `会话 ${source.session_id} · 消息 ${range}`
}

function Turn({ item }: { item: DisplayMessage }) {
  const user = item.role === 'user'
  const assistant = item.role === 'assistant'
  const speaker = user ? (item.navigation ? '接续操作' : '你') : (assistant ? 'RecallWeave' : item.role)
  return (
    <article className={`transcript-turn ${user ? 'transcript-user' : 'transcript-agent'}`} aria-label={speaker}>
      <header className="transcript-speaker">
        {assistant && <img src="/weave.svg" alt="" width="20" height="20" />}
        <span>{speaker}</span>
        {item.inherited && <span className="transcript-inherited">继承自上文</span>}
      </header>
      <MessageText text={item.text} />
      {item.source && <small className="transcript-anchor">选中的原文 · {location(item.source)}</small>}
      {!!item.citations?.length && (
        <details className="transcript-sources">
          <summary>已核对的原文 · {item.citations.length}</summary>
          <ul>{item.citations.map(citation => (
            <li key={`${citation.memory_id}-${citation.session_id}-${citation.seq_start}-${citation.seq_end}`}>
              {citation.title && <span>{citation.title}</span>}
              <small>{location(citation)}</small>
            </li>
          ))}</ul>
        </details>
      )}
    </article>
  )
}

export function Transcript({ messages, pendingQuestion, loading, error, onRetry }: Props) {
  const viewport = useRef<HTMLDivElement>(null)
  const follow = useRef(true)
  const last = messages.at(-1)
  useLayoutEffect(() => {
    if (viewport.current && follow.current) viewport.current.scrollTop = viewport.current.scrollHeight
  }, [last?.key, last?.text, loading, pendingQuestion])

  return (
    <div ref={viewport} className="transcript" onScroll={event => {
      const el = event.currentTarget
      follow.current = el.scrollHeight - el.clientHeight - el.scrollTop < 72
    }}>
      <div className="transcript-column">
        {loading && <p className="transcript-notice" role="status">正在读取对话…</p>}
        {error && (
          <div className="transcript-notice" role="alert">
            <span>{error}</span>
            <button type="button" onClick={onRetry}>重新加载</button>
          </div>
        )}
        <div className="transcript-log" role="log" aria-label="对话记录" aria-live={loading ? 'off' : 'polite'} aria-relevant="additions">
          {messages.map(item => <Turn key={item.key} item={item} />)}
          {pendingQuestion && <Turn item={{ key: 'pending', role: 'user', text: pendingQuestion }} />}
          {pendingQuestion !== undefined && (
            <div className="transcript-pending" role="status">
              <img src="/weave.svg" width="20" height="20" alt="" /> 正在思考…
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
