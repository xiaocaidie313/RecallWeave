export interface Conversation {
  id: number
  title: string
  updated_at: string
  created_at: string
  parent_conversation_id?: number
}

export interface ThoughtSource {
  session_id: number
  seq_start: number
  seq_end: number
}

export type Direction = 'explore' | 'reject' | 'reframe'

export interface Checkpoint {
  conversation_id: number
  text: string
  direction?: Direction
  author: 'user'
  updated_at: string
}

export interface ThinkingContext {
  source?: ThoughtSource
  checkpoint?: Checkpoint
  navigation?: boolean
}

export interface Message {
  id?: number
  session_id: number
  seq: number
  role: string
  content: string
  navigation?: boolean
  thinking?: boolean
  thinking_context?: ThinkingContext
}

export interface History {
  session_id?: number
  messages: Message[]
  inherited_messages: Message[]
}

export interface ImportedSession {
  session_id: number
  message_count: number
  extract_status: 'pending' | 'done' | 'failed' | 'skipped'
}

export interface NoteSummary {
  session_id: number
  name: string
  extract_status: ImportedSession['extract_status']
}

export interface Citation extends ThoughtSource {
  memory_id: number
  title: string
}

export interface ThinkingAnswer {
  answer: string
  citations: Citation[]
  rounds: number
  run_id?: string
  thinking?: ThinkingContext
}

export class ApiError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

const noteSizeLimit = '单个笔记文件最多 20 MiB，请拆分后导入。'
const noteBackendUpdate = '当前后端还不支持本地笔记。请重新启动更新后的后端，再试一次。'

const errorMessages: Record<string, string> = {
  'chat: llm is not configured': '聊天模型尚未配置。请在后端设置模型和 API Key 后重试。',
  'conversation not found': '这段对话已找不到，请刷新对话列表。',
  'question is empty': '请先写下想问的问题。',
  'provide a question, select original messages, or save a thinking checkpoint first': '请先写下一个想继续探讨的问题。',
}

function describeError(path: string, status: number, detail: string) {
  if (status === 404 && !detail && /^\/conversations\/\d+\/notes$/.test(path)) return noteBackendUpdate
  if (/^\/conversations\/\d+\/imports\/file$/.test(path)) {
    if (status === 413) return noteSizeLimit
    if (status === 400) {
      if (detail === 'unknown source tag') return noteBackendUpdate
      if (detail.endsWith('has no importable content')) return '文件中没有可以导入的文字。'
      return '无法读取这份笔记，请确认文件包含 UTF-8 编码的 Markdown 或纯文本。'
    }
  }
  if (/^\/chat\/\d+$/.test(path)) {
    const paymentRequired = status === 402 && detail === 'model provider requires payment'
    const legacyPaymentRequired = status === 500 && /(?:^|: )llm: chat completion failed: POST "[^"]+": 402 Payment Required\b/.test(detail)
    if (paymentRequired || legacyPaymentRequired) {
      return '模型服务商要求付费。请检查后端 API Key 所属账户的余额和所选模型的计费要求，处理后重新发送。'
    }
  }
  if (Object.hasOwn(errorMessages, detail)) return errorMessages[detail]
  if (detail.startsWith('thinking mode is unavailable')) return '思考模式暂时不可用，请检查后端的 thinking-partner 技能和配置。'
  if (status === 400) return '提交的内容有误，请检查后再试。'
  if (status === 404) return '找不到所请求的数据或接口，请刷新并检查后端版本。'
  if (status === 502 || status === 504 || (status === 500 && !detail)) return '暂时连不上 RecallWeave 后端，请确认服务已启动。'
  return `请求未能完成（HTTP ${status}），请检查后端服务并重试。`
}

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  let response: Response
  try {
    response = await fetch(`/api${path}`, options)
  } catch (error) {
    if (options.signal?.aborted) throw error
    throw new ApiError(0, '无法连接 RecallWeave 后端，请确认服务已启动后重试。')
  }
  if (!response.ok) {
    const body = await response.json().catch(() => null)
    const detail = typeof body?.error === 'string' ? body.error : ''
    throw new ApiError(response.status, describeError(path, response.status, detail))
  }
  try {
    return await response.json() as T
  } catch (error) {
    if (options.signal?.aborted) throw error
    throw new ApiError(response.status, '接口没有返回有效的 JSON，请检查前端的 API 代理与后端地址。')
  }
}

function post<T>(path: string, body: unknown) {
  return request<T>(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

export async function listConversations(signal?: AbortSignal): Promise<Conversation[]> {
  const data = await request<{ conversations: Conversation[] | null }>('/conversations/list', { signal })
  return data.conversations ?? []
}

export function createConversation(title = ''): Promise<Conversation> {
  return post('/conversations/create', { title })
}

export function validateNoteFile(file: File): string | undefined {
  if (!/\.(md|txt)$/i.test(file.name)) return '请选择 .md 或 .txt 笔记文件。'
  if (file.size === 0) return '文件为空，请选择有内容的笔记。'
  if (file.size > 20 * 1024 * 1024) return noteSizeLimit
}

export async function importNote(conversationId: number, file: File): Promise<ImportedSession[]> {
  const error = validateNoteFile(file)
  if (error) throw new Error(error)

  const form = new FormData()
  form.set('source_tag', 'local_note')
  form.set('name', file.name)
  if (/\.md$/i.test(file.name)) form.set('format', 'markdown')
  form.set('file', file)
  const data = await request<{ sessions: ImportedSession[] }>(
    `/conversations/${conversationId}/imports/file`, { method: 'POST', body: form },
  )
  return data.sessions
}

export async function listNotes(conversationId: number, signal?: AbortSignal): Promise<NoteSummary[]> {
  const data = await request<{ notes: NoteSummary[] | null }>(`/conversations/${conversationId}/notes`, { signal })
  return data.notes ?? []
}

export async function getNoteMessages(sessionId: number, signal?: AbortSignal): Promise<Message[]> {
  const data = await request<{ messages: Message[] | null }>(`/sessions/${sessionId}/messages`, { signal })
  return data.messages ?? []
}

export async function getHistory(id: number, signal?: AbortSignal): Promise<History> {
  const data = await request<{
    session_id?: number
    messages: Message[]
    inherited_messages: Message[] | null
  }>(`/conversations/${id}/messages`, { signal })
  return { ...data, inherited_messages: data.inherited_messages ?? [] }
}

export async function sendTurn(id: number, question: string, mode: 'thinking' | 'chat', source?: ThoughtSource): Promise<ThinkingAnswer> {
  const data = await post<Omit<ThinkingAnswer, 'citations'> & { citations: Citation[] | null }>(
    `/chat/${id}`, { question, mode, source: mode === 'thinking' ? source : undefined },
  )
  return { ...data, citations: data.citations ?? [] }
}
