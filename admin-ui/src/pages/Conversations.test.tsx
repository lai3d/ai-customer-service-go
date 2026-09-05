import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import type { ConversationSummary } from '../api/types'

// The api module is replaced so these tests are about the page, not the network.
const conversations = vi.fn()
vi.mock('../api/client', () => ({
  api: { conversations: (...args: unknown[]) => conversations(...args) },
  ApiError: class extends Error {},
}))

const { ConversationsPage } = await import('./Conversations')

const rows = (n: number, offset = 0): ConversationSummary[] =>
  Array.from({ length: n }, (_, i) => ({
    conversationId: `conv-${offset + i}`,
    turns: 1,
    startedAt: '2026-09-06T00:00:00Z',
    lastAt: '2026-09-06T00:00:00Z',
    outcomes: ['completed'],
    inputTokens: 10,
    outputTokens: 5,
    tickets: 0,
  }))

afterEach(() => {
  cleanup()
  conversations.mockReset()
})

describe('the conversation list', () => {
  // The defect this exists for: the API returns `total`, the page never passed it to the
  // table, and Ant Design fell back to counting the rows it had been given. With 250
  // conversations the footer stated 20, and the other 230 were gone with no indication
  // that anything was missing.
  it('states the number of conversations the server has, not the number on screen', async () => {
    conversations.mockResolvedValue({ total: 250, conversations: rows(20) })
    render(<ConversationsPage />)
    await waitFor(() => expect(screen.getByText(/conversation\(s\)/)).toBeTruthy())
    expect(screen.getByText('250 conversation(s)')).toBeTruthy()
  })

  it('asks the server for the next page rather than slicing what it has', async () => {
    conversations.mockResolvedValue({ total: 250, conversations: rows(20) })
    render(<ConversationsPage />)
    await waitFor(() => expect(conversations).toHaveBeenCalled())
    expect(conversations).toHaveBeenLastCalledWith(
      expect.objectContaining({ limit: 20, offset: 0 }))

    screen.getByTitle('2').click()
    await waitFor(() =>
      expect(conversations).toHaveBeenLastCalledWith(
        expect.objectContaining({ limit: 20, offset: 20 })))
  })

  it('does not ask for more rows than the API will return', async () => {
    // The API caps a page at 200 and silently substitutes 50 for anything outside its
    // range, so a page size the server will not honour produces a table that disagrees
    // with its own footer.
    conversations.mockResolvedValue({ total: 250, conversations: rows(20) })
    render(<ConversationsPage />)
    await waitFor(() => expect(conversations).toHaveBeenCalled())
    const { limit } = conversations.mock.calls[0]![0] as { limit: number }
    expect(limit).toBeGreaterThan(0)
    expect(limit).toBeLessThanOrEqual(200)
  })
})
