import { useEffect, useRef, useState } from 'react'
import { getNoteMessages, listNotes, type Message, type NoteSummary, type ThoughtSource } from './api'
import './NoteLibrary.css'

type PickPassage = (name: string, source: ThoughtSource) => void

const extraction = { pending: '等待提炼', done: '已提炼', failed: '提炼失败', skipped: '仅保存原文' }

function NoteText({ note, onPick }: { note: NoteSummary; onPick: PickPassage }) {
  const [messages, setMessages] = useState<Message[]>()
  const [error, setError] = useState('')
  const [page, setPage] = useState(0)
  const pageSize = 10
  const scroller = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const abort = new AbortController()
    getNoteMessages(note.session_id, abort.signal).then(rows => {
      if (!abort.signal.aborted) setMessages(rows)
    }).catch(e => { if (!abort.signal.aborted) setError(e.message) })
    return () => abort.abort()
  }, [note.session_id])
  const pageCount = Math.ceil((messages?.length ?? 0) / pageSize)
  function go(next: number) {
    setPage(next)
    scroller.current?.scrollTo({ top: 0 })
  }
  return (
    <section className="note-reader" aria-label="笔记原文">
      <div className="note-reader-heading"><h3>{note.name}</h3>{messages && <span>共 {messages.length} 段</span>}</div>
      <div className="note-passages" ref={scroller}>
        {error ? <p role="alert">{error}</p> : !messages ? <p role="status">正在读取原文…</p> : !messages.length ? <p>这份笔记没有可读的段落。</p> :
          messages.slice(page * pageSize, (page + 1) * pageSize).map(m => (
            <article className="note-passage" key={m.seq}>
              <div className="note-passage-heading"><span>第 {m.seq} 段</span>
                <button type="button" onClick={() => onPick(note.name, { session_id: note.session_id, seq_start: m.seq, seq_end: m.seq })}>从这段接着想</button>
              </div>
              <p>{m.content}</p>
            </article>
          ))}
      </div>
      {pageCount > 1 && <nav className="note-pages" aria-label="原文翻页">
        <button type="button" disabled={page === 0} onClick={() => go(page - 1)}>上一页</button>
        <span>{page + 1} / {pageCount}</span>
        <button type="button" disabled={page + 1 === pageCount} onClick={() => go(page + 1)}>下一页</button>
      </nav>}
    </section>
  )
}

function Notes({ id, onPick }: { id: number; onPick: PickPassage }) {
  const [notes, setNotes] = useState<NoteSummary[]>()
  const [error, setError] = useState('')
  const [selection, setSelection] = useState<number>()
  useEffect(() => {
    const abort = new AbortController()
    listNotes(id, abort.signal).then(rows => {
      if (!abort.signal.aborted) setNotes(rows)
    }).catch(e => { if (!abort.signal.aborted) setError(e.message) })
    return () => abort.abort()
  }, [id])
  if (error) return <p className="notes-empty" role="alert">{error}</p>
  if (!notes) return <p className="notes-empty" role="status">正在读取笔记…</p>
  if (!notes.length) return <p className="notes-empty">这里还没有笔记。点击输入框下方的「导入笔记」，选择 Markdown 或 TXT 文件。</p>
  const selected = notes.find(note => note.session_id === selection) ?? notes[0]
  return (
    <div className="notes-body">
      <nav className="note-list" aria-label="已导入文件">
        {notes.map(note => <button type="button" key={note.session_id} aria-pressed={selected.session_id === note.session_id}
          onClick={() => setSelection(note.session_id)}>
          <strong>{note.name}</strong><small>{extraction[note.extract_status]}</small>
        </button>)}
      </nav>
      <NoteText key={selected.session_id} note={selected} onPick={onPick} />
    </div>
  )
}

export function NoteLibrary({ id, onClose, onPick }: { id: number; onClose: () => void; onPick: PickPassage }) {
  const dialog = useRef<HTMLDialogElement>(null)
  const [version, setVersion] = useState(0)
  useEffect(() => {
    const node = dialog.current!
    node.showModal()
    return () => node.close()
  }, [])
  return (
    <dialog className="notes-dialog" ref={dialog} aria-labelledby="notes-title" onCancel={event => { event.preventDefault(); onClose() }}>
      <header className="notes-heading">
        <h2 id="notes-title">当前对话的笔记</h2>
        <button type="button" onClick={() => setVersion(current => current + 1)}>刷新</button>
        <button type="button" onClick={onClose}>关闭</button>
      </header>
      <Notes key={version} id={id} onPick={onPick} />
    </dialog>
  )
}
