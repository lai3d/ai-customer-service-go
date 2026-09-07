import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import type { WhoAmI } from './api/types'

// Which tabs an account is offered.
//
// The server refuses a platform account on every customer-facing route, reads included, so a
// UI that offers it those tabs is a UI whose every tab answers 403 — and one that offers a
// tenant operator the Tenants tab is the same mistake pointing the other way. Neither is a
// security control: the server decides. It is the difference between a surface somebody can
// use and one they have to learn to avoid.
const whoami = vi.fn()
const overview = vi.fn()
const tenants = vi.fn()

vi.mock('./api/client', () => ({
  api: {
    whoami: () => whoami(),
    overview: () => overview(),
    tenants: () => tenants(),
  },
  ApiError: class extends Error {},
  apiBase: () => 'http://localhost:8081/api/admin/v1',
  setToken: () => {},
  token: () => 'a-token',
}))

const { App } = await import('./App')

afterEach(() => {
  cleanup()
  whoami.mockReset()
  overview.mockReset()
  tenants.mockReset()
})

const platform: WhoAmI = {
  name: 'root', role: 'platform', canWrite: false, tenantId: '', isPlatform: true,
}
const operator: WhoAmI = {
  name: 'alex', role: 'operator', canWrite: true, tenantId: 'acme', isPlatform: false,
}

describe('the tabs an account is offered', () => {
  it('gives a platform account the one page it can use, and none of the others', async () => {
    whoami.mockResolvedValue(platform)
    tenants.mockResolvedValue({ tenants: [] })
    render(<App />)

    await waitFor(() => expect(screen.getByRole('tab', { name: 'Tenants' })).toBeTruthy())
    for (const label of ['Overview', 'Conversations', 'Tickets', 'Knowledge', 'Feedback', 'Audit']) {
      expect(screen.queryByRole('tab', { name: label })).toBeNull()
    }
  })

  it('gives a tenant operator every page except Tenants', async () => {
    whoami.mockResolvedValue(operator)
    overview.mockResolvedValue({
      since: '2026-09-07T00:00:00Z', turnsByOutcome: {}, conversations: 0,
      inputTokens: 0, outputTokens: 0, costUsd: 0, tickets: {},
    })
    render(<App />)

    await waitFor(() => expect(screen.getByRole('tab', { name: 'Overview' })).toBeTruthy())
    for (const label of ['Conversations', 'Tickets', 'Knowledge', 'Feedback', 'Audit']) {
      expect(screen.queryByRole('tab', { name: label })).toBeTruthy()
    }
    expect(screen.queryByRole('tab', { name: 'Tenants' })).toBeNull()
  })

  it('says which tenant is being shown, and says nothing when there is none', async () => {
    whoami.mockResolvedValue(operator)
    overview.mockResolvedValue({
      since: '2026-09-07T00:00:00Z', turnsByOutcome: {}, conversations: 0,
      inputTokens: 0, outputTokens: 0, costUsd: 0, tickets: {},
    })
    const { unmount } = render(<App />)
    // An operations surface that does not name the tenant it is showing makes "is this the
    // right customer" a question about the title bar.
    await waitFor(() => expect(screen.getByText('acme')).toBeTruthy())
    unmount()

    whoami.mockResolvedValue(platform)
    tenants.mockResolvedValue({ tenants: [] })
    render(<App />)
    await waitFor(() => expect(screen.getByRole('tab', { name: 'Tenants' })).toBeTruthy())
    expect(screen.queryByText('acme')).toBeNull()
  })
})
