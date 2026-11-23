import { useEffect, useRef, useState } from 'react'
import type { Conversation } from './api'
import './ConversationSidebar.css'

interface SidebarProps {
  conversations: Conversation[]
  activeId: number | null
  loading: boolean
  error?: string | null
  onNew: () => void
  onSelect: (id: number) => void
  onRetry: () => void
}

function byDate(conversations: Conversation[], query: string) {
  const keyword = query.trim().toLocaleLowerCase()
  const groups = new Map<string, Conversation[]>()
  const today = new Date()
  const todayDay = Date.UTC(today.getFullYear(), today.getMonth(), today.getDate())
  const rows = conversations
    .filter(item => !keyword || (item.title || '新对话').toLocaleLowerCase().includes(keyword))
    .map(item => ({ item, time: Date.parse(item.updated_at) || Date.parse(item.created_at) || 0 }))
    .sort((a, b) => b.time - a.time || b.item.id - a.item.id)

  for (const { item, time } of rows) {
    const date = new Date(time)
    const days = (todayDay - Date.UTC(date.getFullYear(), date.getMonth(), date.getDate())) / 86_400_000
    let label = '更早'
    if (days === 0) label = '今天'
    else if (days === 1) label = '昨天'
    else if (days > 1 && days < 7) label = '过去 7 天'
    else if (days > 0 && days < 30) label = '过去 30 天'
    const group = groups.get(label)
    if (group) group.push(item)
    else groups.set(label, [item])
  }
  return groups
}

const strokes = {
  search: <><circle cx="10.5" cy="10.5" r="6.5" /><path d="m16 16 4.5 4.5" /></>,
  compose: <><path d="M13 5H6a2 2 0 0 0-2 2v11a2 2 0 0 0 2 2h11a2 2 0 0 0 2-2v-7" /><path d="m10 14 1-4 7-7a1.4 1.4 0 0 1 2 0l1 1a1.4 1.4 0 0 1 0 2l-7 7-4 1Z" /></>,
  workspace: <><rect x="3" y="4" width="18" height="16" rx="3" /><path d="M9 4v16" /></>,
}

function SidebarIcon({ name }: { name: keyof typeof strokes }) {
  return <svg aria-hidden="true" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round">{strokes[name]}</svg>
}

export default function ConversationSidebar({ conversations, activeId, loading, error, onNew, onSelect, onRetry }: SidebarProps) {
  const search = useRef<HTMLInputElement>(null)
  const searchToggle = useRef<HTMLButtonElement>(null)
  const [searchOpen, setSearchOpen] = useState(false)
  const [query, setQuery] = useState('')
  const groups = byDate(conversations, query)

  useEffect(() => {
    if (searchOpen) search.current?.focus()
  }, [searchOpen])

  function visit(action: () => void) {
    setQuery('')
    setSearchOpen(false)
    action()
  }

  return (
    <aside className="rw-side" aria-label="对话导航">
      <div className="rw-side__brand-row">
        <div className="rw-side__brand"><img src="/weave.svg" width="28" height="28" alt="" /><span>RecallWeave</span></div>
        <button ref={searchToggle} type="button" className="rw-side__icon" aria-label="搜索历史对话" title="搜索历史对话"
          aria-expanded={searchOpen} aria-controls="conversation-history-search" onClick={() => { setSearchOpen(!searchOpen); setQuery('') }}>
          <SidebarIcon name="search" />
        </button>
      </div>
      <div id="conversation-history-search" className="rw-side__search" hidden={!searchOpen}>
        <input ref={search} type="search" aria-label="按标题搜索最近对话" placeholder="搜索最近对话的标题" value={query} onChange={event => setQuery(event.target.value)}
          onKeyDown={event => {
            if (event.key === 'Escape') {
              setSearchOpen(false)
              setQuery('')
              searchToggle.current?.focus()
            }
          }} />
      </div>
      <button type="button" className="rw-side__compose" onClick={() => visit(onNew)}><SidebarIcon name="compose" />新对话</button>
      <nav className="rw-side__history" aria-labelledby="conversation-history-heading">
        <h2 id="conversation-history-heading">最近对话</h2>
        {loading && <p className="rw-side__status" role="status">正在加载对话…</p>}
        {error && <div className="rw-side__status" role="alert"><p>{error}</p><button type="button" onClick={onRetry}>重新加载</button></div>}
        {!loading && !error && groups.size === 0 && <p className="rw-side__status" role="status">{query.trim() ? '没有匹配的对话' : '还没有对话'}</p>}
        {Array.from(groups, ([label, items]) => (
          <div className="rw-side__group" key={label}>
            <h3>{label}</h3>
            <ul>
              {items.map(item => (
                <li key={item.id}><button type="button" title={item.title || '新对话'} aria-current={item.id === activeId ? 'page' : undefined}
                  onClick={() => visit(() => onSelect(item.id))}>{item.title || '新对话'}</button></li>
              ))}
            </ul>
          </div>
        ))}
      </nav>
      <div className="rw-side__workspace"><span className="rw-side__space-icon"><SidebarIcon name="workspace" /></span>个人工作区</div>
    </aside>
  )
}
