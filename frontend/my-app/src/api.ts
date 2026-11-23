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

const errorMessages: Record<string, string> = {
  'chat: llm is not configured': '聊天模型尚未配置。请在后端设置模型和 API Key 后重试。',
  'conversation not found': '这段对话已找不到，请刷新对话列表。',
  'question is empty': '请先写下想问的问题。',
  'provide a question, select original messages, or save a thinking checkpoint first': '请先写下一个想继续探讨的问题。',
}

function describeError(status: number, detail: string) {
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
    throw new ApiError(response.status, describeError(response.status, detail))
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

export async function getHistory(id: number, signal?: AbortSignal): Promise<History> {
  const data = await request<{
    session_id?: number
    messages: Message[]
    inherited_messages: Message[] | null
  }>(`/conversations/${id}/messages`, { signal })
  return { ...data, inherited_messages: data.inherited_messages ?? [] }
}

export async function sendTurn(id: number, question: string, mode: 'thinking' | 'chat'): Promise<ThinkingAnswer> {
  const data = await post<Omit<ThinkingAnswer, 'citations'> & { citations: Citation[] | null }>(
    `/chat/${id}`, { question, mode },
  )
  return { ...data, citations: data.citations ?? [] }
}
