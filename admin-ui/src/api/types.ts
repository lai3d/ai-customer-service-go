// The shapes internal/admin and internal/ticket serialise. Hand-written rather than
// generated: seven endpoints do not justify a code generator in the build, and a type
// that is written down is a type someone reads.
//
// TypeScript cannot check these against the Go structs -- nothing here reaches the server
// at compile time. What it does check is that this file and every component agree, which
// is the drift that actually happens once four views read the same payload.

export type Role = 'viewer' | 'operator' | 'platform'

export interface WhoAmI {
  name: string
  role: Role
  canWrite: boolean
  /** Whose customers this account can see. Empty for the platform role, which sees none. */
  tenantId: string
  /**
   * The tenant-less role that creates tenants and issues their keys.
   *
   * Sent by the server rather than derived from `role` here: which roles are tenant-less is
   * the server's decision, and a second copy of that rule in the page is a second place to
   * change it. The server refuses a platform account on every customer-facing route, so the
   * page hides those tabs rather than offering six that each answer 403.
   */
  isPlatform: boolean
}

/** A tenant of this service: one product, one corpus, one set of customers. */
export interface Tenant {
  id: string
  name: string
  disabled: boolean
  createdAt: string
  createdBy: string
  disabledAt?: string
}

/**
 * An API key. `secret` is present exactly once, in the response that issues it, and is not
 * stored anywhere — so a page that loses it cannot ask for it again.
 */
export interface TenantKey {
  keyId: string
  tenantId: string
  label: string
  createdAt: string
  createdBy: string
  lastUsedAt?: string
  revokedAt?: string
  secret?: string
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
  /** Reported-wrong answers nobody has acted on. The number that matters is this one and
   *  not the total: feedback nobody clears is a suggestion box. */
  openFeedback: number
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

export interface KnowledgeEntry {
  entryId: string
  language: string
  category: string
  question: string
  answer: string
  deleted: boolean
  updatedAt: string
  updatedBy: string
}

export interface CorpusVersion {
  version: string
  source: string
  documents: number
  createdAt: string
  createdBy: string
  note?: string
  active: boolean
}

/** The bound the server enforces on an answer. Mirrored so the editor can say so before
 *  the request rather than after it; the server is still the one that decides. */
export const MAX_ANSWER_LENGTH = 4000

export type FeedbackSource = 'customer' | 'operator'
export type FeedbackVerdict = 'helpful' | 'wrong' | 'unclear'

export interface FeedbackItem {
  turnId: string
  conversationId: string
  source: FeedbackSource
  verdict: FeedbackVerdict
  note?: string
  actor: string
  at: string
  question: string
  answer?: string
  model?: string
  outcome: string
  /** What was retrieved for that turn: where a wrong answer usually starts. */
  entries?: string[]
  handled: boolean
  handledAt?: string
}

