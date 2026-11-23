import { useCallback, useEffect, useRef, useState } from 'react'
import { createConversation, getHistory, importNote, listConversations, sendTurn, validateNoteFile } from './api'
import type { Conversation, Message, ThoughtSource } from './api'
import type { DisplayMessage } from './Transcript'

export type ChatMode = 'thinking' | 'chat'

interface Room {
  draft: string
  mode: ChatMode
  messages?: DisplayMessage[]
  pending?: string
  sendError?: string
  historyError?: string
  importing?: boolean
  importNotice?: { text: string; error?: boolean }
  passage?: { source: ThoughtSource; name: string }
}

const empty: Room = { draft: '', mode: 'thinking' }
const fresh = (): Room => ({ ...empty, messages: [] })

function routeId() {
  const match = /^#chat\/(\d+)$/.exec(window.location.hash)
  const id = match ? Number(match[1]) : 0
  return Number.isSafeInteger(id) && id > 0 ? id : 0
}

function describe(error: unknown) {
  return error instanceof Error ? error.message : '请求失败，请重试。'
}

function display(message: Message, inherited = false): DisplayMessage {
  const navigation = message.navigation || message.thinking_context?.navigation
  const source = message.thinking_context?.source
  return {
    key: `${message.session_id}/${message.seq}`, role: message.role,
    text: navigation && source ? '从选中的原文接着想。' : message.content,
    navigation, source, inherited,
  }
}

// Keep drafts and in-flight turns scoped to their room when the sidebar changes.
// The address holds only the conversation ID. History comes from the server.
export function useChat() {
  const [id, setId] = useState(routeId)
  const selected = useRef(id)
  const [rooms, setRooms] = useState<Record<number, Room>>(() => ({ 0: fresh() }))
  const room = rooms[id] ?? empty
  const locks = useRef(new Set<number>())
  const [conversations, setConversations] = useState<Conversation[]>([])
  const [listState, setListState] = useState<'loading' | 'ready' | 'error'>('loading')
  const [listError, setListError] = useState('')
  const [listAttempt, setListAttempt] = useState(0)
  const [historyAttempt, setHistoryAttempt] = useState(0)

  const update = useCallback((key: number, change: (old: Room) => Room) => {
    setRooms(previous => ({ ...previous, [key]: change(previous[key] ?? empty) }))
  }, [])

  function navigate(next: number, replace = false) {
    selected.current = next
    setId(next)
    const path = window.location.pathname + window.location.search + (next ? `#chat/${next}` : '')
    if (replace) window.history.replaceState(null, '', path)
    else if (window.location.hash !== (next ? `#chat/${next}` : '')) window.history.pushState(null, '', path)
  }

  useEffect(() => {
    const follow = () => { selected.current = routeId(); setId(selected.current) }
    window.addEventListener('popstate', follow)
    window.addEventListener('hashchange', follow)
    return () => {
      window.removeEventListener('popstate', follow)
      window.removeEventListener('hashchange', follow)
    }
  }, [])

  useEffect(() => {
    const abort = new AbortController()
    listConversations(abort.signal).then(items => {
      if (abort.signal.aborted) return
      setConversations(items)
      setListState('ready')
    }).catch(error => {
      if (abort.signal.aborted) return
      setListState('error')
      setListError(describe(error))
    })
    return () => abort.abort()
  }, [listAttempt])

  const needsHistory = id > 0 && room.messages === undefined
  useEffect(() => {
    if (!needsHistory) return
    const abort = new AbortController()
    getHistory(id, abort.signal).then(history => {
      if (abort.signal.aborted) return
      const messages = [...history.inherited_messages.map(m => display(m, true)), ...history.messages.map(m => display(m))]
      const lastTurn = history.messages.findLast(m => m.role === 'user')
      update(id, old => ({ ...old, messages, historyError: undefined, mode: lastTurn ? (lastTurn.thinking ? 'thinking' : 'chat') : old.mode }))
    }).catch(error => {
      if (!abort.signal.aborted) update(id, old => ({ ...old, historyError: describe(error) }))
    })
    return () => abort.abort()
  }, [id, needsHistory, historyAttempt, update])

  function retryList() {
    setListState('loading')
    setListAttempt(n => n + 1)
  }

  function retryHistory() {
    update(id, old => ({ ...old, historyError: undefined }))
    setHistoryAttempt(n => n + 1)
  }

  async function ensureConversation(title: string) {
    if (id) return id
    const created = await createConversation(Array.from(title).slice(0, 40).join(''))
    locks.current.add(created.id)
    locks.current.delete(0)
    setRooms(previous => ({ ...previous, 0: fresh(), [created.id]: previous[0] ?? fresh() }))
    setConversations(old => [created, ...old.filter(c => c.id !== created.id)])
    if (selected.current === 0) navigate(created.id, true)
    retryList()
    return created.id
  }

  async function uploadNote(file: File) {
    if (locks.current.has(id) || room.messages === undefined) return
    const invalid = validateNoteFile(file)
    if (invalid) {
      update(id, old => ({ ...old, importNotice: { text: invalid, error: true } }))
      return
    }
    locks.current.add(id)
    let destination = id
    update(id, old => ({ ...old, importing: true, importNotice: { text: `正在导入「${file.name}」…` } }))
    try {
      destination = await ensureConversation(file.name)
      const sessions = await importNote(destination, file)
      const count = sessions.reduce((total, note) => total + note.message_count, 0)
      update(destination, old => ({ ...old, importing: false, importNotice: { text: `已保存「${file.name}」，共 ${count} 段原文。` } }))
    } catch (error) {
      update(destination, old => ({ ...old, importing: false, importNotice: { text: describe(error), error: true } }))
    } finally {
      locks.current.delete(destination)
    }
  }

  async function send() {
    const question = room.draft.trim()
    const source = room.passage?.source
    if ((!question && !source) || room.messages === undefined || locks.current.has(id)) return
    let destination = id
    locks.current.add(id)
    update(id, old => ({ ...old, draft: '', pending: question, sendError: undefined }))
    try {
      // A lost HTTP response can arrive after the backend committed the turn.
      // Check persisted history before a user retries that same question.
      if (id > 0 && room.sendError) {
        const history = await getHistory(id)
        const saved = [...history.inherited_messages.map(m => display(m, true)), ...history.messages.map(m => display(m))]
        const added = saved.slice(room.messages.length)
        if (added.some((m, i) => m.role === 'user' && (question ? m.text === question : m.navigation) && added[i + 1]?.role === 'assistant' &&
          m.source?.session_id === source?.session_id && m.source?.seq_start === source?.seq_start && m.source?.seq_end === source?.seq_end)) {
          update(id, old => ({ ...old, pending: undefined, passage: undefined, messages: saved }))
          return
        }
        if (added.length) update(id, old => ({ ...old, messages: saved }))
      }
      destination = await ensureConversation(question.split(/\r?\n/, 1)[0])
      const answer = await sendTurn(destination, question, room.mode, source)
      const turn = answer.run_id ?? crypto.randomUUID()
      update(destination, old => ({
        ...old, pending: undefined, sendError: undefined, passage: undefined,
        messages: [...(old.messages ?? []),
          { key: turn + '-question', role: 'user', text: question || '从选中的原文接着想。', source, navigation: !question },
          { key: turn, role: 'assistant', text: answer.answer, citations: answer.citations },
        ],
      }))
    } catch (error) {
      update(destination, old => ({ ...old, pending: undefined, draft: question, sendError: describe(error) }))
    } finally {
      locks.current.delete(destination)
    }
  }

  return {
    id, room, conversations, listState, listError, retryList, retryHistory, navigate, send, uploadNote,
    choosePassage: (name: string, source: ThoughtSource) => update(id, old => ({
      ...old, mode: 'thinking', passage: { name, source }, sendError: undefined,
    })),
    clearPassage: () => update(id, old => ({ ...old, passage: undefined, sendError: undefined })),
    setDraft: (draft: string) => update(id, old => ({ ...old, draft, sendError: undefined })),
    setMode: (mode: ChatMode) => update(id, old => ({ ...old, mode, passage: mode === 'thinking' ? old.passage : undefined, sendError: undefined })),
  }
}
