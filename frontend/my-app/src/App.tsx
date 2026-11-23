import { useEffect, useRef, useState } from 'react'
import { NoteLibrary } from './NoteLibrary'
import ConversationSidebar from './ConversationSidebar'
import { Transcript } from './Transcript'
import { useChat, type ChatMode } from './useChat'
import './App.css'

const icons = {
  arrow: <path d="M12 19V5m-6 6 6-6 6 6" />,
  attach: <path d="m8 13 6-6a2.4 2.4 0 0 1 3.4 3.4l-8 8a4.2 4.2 0 0 1-6-6l8.5-8.5a5.7 5.7 0 0 1 8.1 8.1l-8.1 8.1" />,
  chevron: <path d="m7 10 5 5 5-5" />,
  path: <path d="m10 7 5 5-5 5" />,
  chat: <path d="M19 16a3 3 0 0 0 2-3V7a3 3 0 0 0-3-3H6a3 3 0 0 0-3 3v9a2 2 0 0 0 2 2h1v3l4-3h5" />,
  branch: <><circle cx="7" cy="5" r="2" /><circle cx="7" cy="19" r="2" /><circle cx="17" cy="7" r="2" /><path d="M7 7v10m0-3c5 0 10-1 10-5" /></>,
  past: <><path d="M4 11a8 8 0 1 1 2.4 6.7M4 5v6h6" /><path d="M12 8v4l2.5 1.5" /></>,
  widen: <><path d="M4 18 18 5M11 5h7v7M4 12v6h6" /></>,
  deepen: <><circle cx="10" cy="10" r="5.8" /><path d="m14.5 14.5 5 5M7.5 10h5M10 7.5v5" /></>,
}

function Icon({ name, size = 18 }: { name: keyof typeof icons, size?: number }) {
  return <svg aria-hidden="true" viewBox="0 0 24 24" width={size} height={size} fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round">{icons[name]}</svg>
}

const starters = [
  { icon: 'past', title: '回看一个想法', prompt: '最近的记录里，有哪些想法是我反复提起的？' },
  { icon: 'widen', title: '换个角度', prompt: '帮我从过去的记录中，找两件看似无关、却可能有联系的事。' },
  { icon: 'deepen', title: '再想深一点', prompt: '我有一个还没想清楚的问题，陪我一起找出它背后的假设。' },
] as const

export default function App() {
  const chat = useChat()
  const { room } = chat
  const editor = useRef<HTMLTextAreaElement>(null)
  const fileInput = useRef<HTMLInputElement>(null)
  const [openLibrary, setOpenLibrary] = useState<number | null>(null)
  const channel = chat.conversations.find(item => item.id === chat.id)
  const title = channel?.title || (chat.id ? '当前对话' : '新对话')
  const welcome = room.messages?.length === 0 && room.pending === undefined
  const waiting = room.pending !== undefined
  const editorDisabled = waiting || !!room.importing || room.messages === undefined
  const statusLabel = { loading: '正在连接', ready: '服务已连接', error: '连接异常' }[chat.listState]

  useEffect(() => { document.title = title + ' · RecallWeave' }, [title])

  function newChat() {
    chat.navigate(0)
    requestAnimationFrame(() => editor.current?.focus())
  }

  const composer = (
    <div className="compose-wrap">
      {room.sendError && <div role="alert" className="send-error"><span>{room.sendError}</span><button type="button" onClick={() => void chat.send()}>重试发送</button></div>}
      {room.importNotice && <div key={room.importNotice.text} className={'note-notice' + (room.importNotice.error ? ' note-error' : '')}
        role={room.importNotice.error ? 'alert' : 'status'}>{room.importNotice.text}</div>}
      <div className="compose-box">
        {room.passage && <div className="chosen-passage">
          <span>选中的原文 · {room.passage.name} · 第 {room.passage.source.seq_start} 段</span>
          <button type="button" aria-label="取消选中的原文" disabled={editorDisabled} onClick={chat.clearPassage}>×</button>
        </div>}
        <textarea ref={editor} rows={2} aria-label="输入消息" placeholder={room.passage ? '按 Enter 从这段原文接着想，也可以写下自己的问题…' : welcome ? '写下一个问题，或还没想完的念头…' : '继续这段对话…'}
          value={room.draft} disabled={editorDisabled} onChange={event => chat.setDraft(event.target.value)}
          onKeyDown={event => {
            if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing && event.nativeEvent.keyCode !== 229) {
              event.preventDefault()
              void chat.send()
            }
          }} />
        <div className="compose-tools">
          <div className="compose-actions">
            <label className="mode-choice">
              <Icon name={room.mode === 'thinking' ? 'branch' : 'chat'} size={15} />
              <select aria-label="对话模式" value={room.mode} disabled={editorDisabled} onChange={event => chat.setMode(event.target.value as ChatMode)}>
                <option value="thinking">接着想</option>
                <option value="chat">普通对话</option>
              </select>
              <Icon name="chevron" size={13} />
            </label>
            <input ref={fileInput} type="file" accept=".md,.txt" aria-label="选择笔记文件" hidden onChange={event => {
              const file = event.currentTarget.files?.[0]
              event.currentTarget.value = ''
              if (file) void chat.uploadNote(file)
            }} />
            <button type="button" className="note-tool" disabled={editorDisabled} title="导入 Markdown 或 TXT 文件"
              onClick={() => fileInput.current?.click()}><Icon name="attach" size={15} />导入笔记</button>
            {chat.id > 0 && <button type="button" className="note-tool" disabled={editorDisabled} onClick={() => setOpenLibrary(chat.id)}>查看笔记</button>}
          </div>
          <div className="send-tools">
            <span className="keyboard-tip">Shift + Enter 换行</span>
            <button type="button" className="send-button" aria-label="发送消息" title="发送消息 (Enter)" disabled={editorDisabled || (!room.draft.trim() && !room.passage)} onClick={() => void chat.send()}>
              <Icon name="arrow" size={20} />
            </button>
          </div>
        </div>
      </div>
    </div>
  )

  return (
    <div className="chat-shell">
      <ConversationSidebar conversations={chat.conversations} activeId={chat.id || null} loading={chat.listState === 'loading'}
        error={chat.listState === 'error' ? chat.listError : undefined}
        onSelect={chat.navigate} onNew={newChat} onRetry={chat.retryList} />
      <main className="chat-main">
        <header className="room-header">
          <div className="room-location"><Icon name="chat" size={17} /><span className="room-parent">对话</span><Icon name="path" size={13} /><strong title={title}>{title}</strong></div>
          <span className={'api-status status-' + chat.listState} role="status"><i aria-hidden="true" />{statusLabel}</span>
        </header>
        {welcome ? (
          <div className="welcome-scroll">
            <section className="welcome" aria-labelledby="welcome-heading">
              <div className="welcome-intro">
                <img className="welcome-mark" src="/weave.svg" width="55" height="55" alt="" />
                <h1 id="welcome-heading">有什么想接着聊的？</h1>
                <p>从一个新问题开始，或沿着过去的想法继续。</p>
              </div>
              {composer}
              <div className="starter-options" aria-label="提问建议">
                {starters.map(item => <button type="button" key={item.title} onClick={() => { chat.setDraft(item.prompt); editor.current?.focus() }}>
                  <Icon name={item.icon} size={15} />{item.title}
                </button>)}
              </div>
            </section>
          </div>
        ) : (
          <>
            <Transcript key={chat.id} messages={room.messages ?? []} pendingQuestion={room.pending}
              loading={room.messages === undefined && !room.historyError} error={room.historyError} onRetry={chat.retryHistory} />
            <div className="conversation-footer">{composer}</div>
          </>
        )}
      </main>
      {openLibrary !== null && chat.id === openLibrary && <NoteLibrary key={chat.id} id={chat.id} onClose={() => setOpenLibrary(null)}
        onPick={(name, source) => {
          chat.choosePassage(name, source)
          setOpenLibrary(null)
          requestAnimationFrame(() => editor.current?.focus())
        }} />}
    </div>
  )
}
