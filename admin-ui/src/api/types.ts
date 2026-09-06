// The shapes internal/admin and internal/ticket serialise. Hand-written rather than
// generated: seven endpoints do not justify a code generator in the build, and a type
// that is written down is a type someone reads.
//
// TypeScript cannot check these against the Go structs -- nothing here reaches the server
// at compile time. What it does check is that this file and every component agree, which
// is the drift that actually happens once four views read the same payload.

export type Role = 'viewer' | 'operator'

export interface WhoAmI {
  name: string
  role: Role
  canWrite: boolean
}

export interface Overview {
  since: string
  turnsByOutcome: Record<string, number>
  conversations: number
  inputTokens: number
  outputTokens: number
  costUsd: number
  /** Turns whose model had no price entry. A flat cost is otherwise indistinguishable
   *  from a cheap week, which is the whole reason the server counts these. */
  unpricedTurns: number
  tickets: Record<string, number>
  /** Ticket notifications that never reached their destination in the window. A
   *  notification fails silently by nature, so this is the number that makes "nobody told
   *  us" visible rather than deniable. */
  undeliveredHandoffs: number
}

export interface ConversationSummary {
  conversationId: string
  turns: number
  startedAt: string
  lastAt: string
  outcomes: string[]
  inputTokens: number
  outputTokens: number
  tickets: number
}

export interface Passage {
  entryId: string
  language: string
  score: number
  question: string
}

export interface ToolCall {
  name: string
  outcome: string
}

export interface Turn {
  id: string
  startedAt: string
  endedAt?: string
  outcome: string
  question: string
  reply?: string
  model?: string
  modelCalls: number
  inputTokens: number
  outputTokens: number
  costUsd?: number
  traceId?: string
  detail?: string
  passages?: Passage[]
  toolCalls?: ToolCall[]
}

export type TicketState = 'OPEN' | 'IN_PROGRESS' | 'RESOLVED' | 'CLOSED'

export interface Ticket {
  ticketNumber: string
  conversationId: string
  category: string
  summary: string
  orderNumber?: string
  state: TicketState
  assignee?: string
  resolution?: string
  version: number
  createdAt: string
  updatedAt: string
}

export interface TicketEvent {
  at: string
  actor: string
  action: string
  detail?: string
}

export interface AuditEntry {
  at: string
  actor: string
  action: string
  object: string
  outcome: string
  detail?: string
}

export interface TicketPatch {
  expectedVersion: number
  state?: TicketState
  assignee?: string
  resolution?: string
  reason?: string
  note?: string
}

// The server's state machine, mirrored so the UI offers only reachable moves. It is a
// mirror and not the authority: the server rejects an impossible move with 422 whatever
// this says, and TicketStateMachine in the tests asserts the two agree.
export const NEXT_STATES: Record<TicketState, TicketState[]> = {
  OPEN: ['IN_PROGRESS', 'CLOSED'],
  IN_PROGRESS: ['RESOLVED', 'CLOSED'],
  RESOLVED: ['IN_PROGRESS', 'CLOSED'],
  CLOSED: ['IN_PROGRESS'],
}
