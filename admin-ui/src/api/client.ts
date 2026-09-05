import type {
  AuditEntry, ConversationSummary, Overview, Ticket, TicketEvent, TicketPatch, Turn, WhoAmI,
} from './types'

// Written by the container at start-up (public/config.js). Reading it through a function
// rather than at module load keeps a missing config.js from turning into a blank page
// with nothing in the console.
declare global {
  interface Window {
    __ADMIN_CONFIG__?: { apiBase?: string }
  }
}

export function apiBase(): string {
  const base = window.__ADMIN_CONFIG__?.apiBase ?? import.meta.env.VITE_API_BASE ?? ''
  return base.replace(/\/$/, '')
}

// The token is kept in sessionStorage, not localStorage: it reads every customer
// conversation in the database, and a closed tab should not leave it on the machine.
// Every accessor is wrapped, because a browser configured to block site data throws here
// rather than returning null.
const TOKEN_KEY = 'ops-token'

export function token(): string {
  try {
    return sessionStorage.getItem(TOKEN_KEY) ?? ''
  } catch {
    return ''
  }
}

export function setToken(value: string): void {
  try {
    if (value) sessionStorage.setItem(TOKEN_KEY, value)
    else sessionStorage.removeItem(TOKEN_KEY)
  } catch {
    /* a private window; the session still works, it just will not survive a reload */
  }
}

/** Thrown for every non-2xx response. `status` is what the callers switch on -- 401 to
 *  ask for the token again, 409 to tell an operator someone else got there first, 422 to
 *  show the server's own words about why a transition was refused. */
export class ApiError extends Error {
  constructor(readonly status: number, message: string) {
    super(message)
    this.name = 'ApiError'
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  let res: Response
  try {
    res = await fetch(apiBase() + '/api/admin/v1' + path, {
      ...init,
      headers: {
        ...(init.headers ?? {}),
        Authorization: 'Bearer ' + token(),
        ...(init.body ? { 'Content-Type': 'application/json' } : {}),
      },
    })
  } catch (cause) {
    // A cross-origin failure reaches the page as an opaque TypeError with no status and
    // no body -- the browser deliberately tells the page nothing. Saying so here is the
    // difference between a five-minute fix and an hour in the network pane.
    throw new ApiError(0,
      `could not reach ${apiBase() || 'the API'}. If the API is up, this is usually its ` +
      `ADMIN_CORS_ORIGINS not listing ${window.location.origin}. (${String(cause)})`)
  }
  if (res.status === 401) {
    setToken('')
    throw new ApiError(401, 'That token was not accepted.')
  }
  if (!res.ok) {
    throw new ApiError(res.status, (await res.text()).trim() || `HTTP ${res.status}`)
  }
  return res.status === 204 ? (null as T) : ((await res.json()) as T)
}

export const api = {
  whoami: () => request<WhoAmI>('/whoami'),
  overview: (hours: number) => request<Overview>(`/overview?hours=${hours}`),
  conversations: (params: { outcome?: string; q?: string; limit?: number; offset?: number }) =>
    request<{ total: number; conversations: ConversationSummary[] | null }>(
      '/conversations?' + new URLSearchParams(clean(params)).toString()),
  conversation: (id: string) =>
    request<{ conversationId: string; turns: Turn[] }>(`/conversations/${encodeURIComponent(id)}`),
  tickets: (params: {
    state?: string; assignee?: string; conversationId?: string
    limit?: number; offset?: number
  }) =>
    request<{ total: number; tickets: Ticket[] | null }>(
      '/tickets?' + new URLSearchParams(clean(params)).toString()),
  ticket: (number: string) =>
    request<{ ticket: Ticket; history: TicketEvent[] | null }>(
      `/tickets/${encodeURIComponent(number)}`),
  updateTicket: (number: string, patch: TicketPatch) =>
    request<Ticket>(`/tickets/${encodeURIComponent(number)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    }),
  audit: (params: { limit: number; offset: number }) =>
    request<{ total: number; entries: AuditEntry[] | null }>(
      `/audit?limit=${params.limit}&offset=${params.offset}`),
}

function clean(params: Record<string, string | number | undefined>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') out[k] = String(v)
  }
  return out
}
