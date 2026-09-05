import { useState } from 'react'
import type { TablePaginationConfig } from 'antd'

// The server's cap. internal/admin/store.go and internal/ticket/admin.go both replace a
// limit outside 1..200 with 50 rather than failing, so a page size the server will not
// honour gives a table that quietly disagrees with its own footer.
export const MAX_PAGE_SIZE = 200

export interface Paging {
  page: number
  pageSize: number
  /** What to send the API. */
  window: { limit: number; offset: number }
  /** What to give antd's Table, given the total the API reported. */
  pagination: (total: number, showTotal: (n: number) => string) => TablePaginationConfig
  /** Filters change what is being counted, so they must reset to the first page. */
  reset: () => void
}

export function usePaging(initialPageSize = 20): Paging {
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(Math.min(initialPageSize, MAX_PAGE_SIZE))

  return {
    page,
    pageSize,
    window: { limit: pageSize, offset: (page - 1) * pageSize },
    pagination: (total, showTotal) => ({
      current: page,
      pageSize,
      // The whole point. Without an explicit total antd counts the rows it was handed,
      // so the footer states the size of the page and the rest of the data does not
      // exist as far as anyone reading the screen is concerned.
      total,
      showSizeChanger: true,
      pageSizeOptions: [20, 50, 100, MAX_PAGE_SIZE],
      showTotal,
      onChange: (next, size) => {
        setPage(next)
        setPageSize(Math.min(size, MAX_PAGE_SIZE))
      },
    }),
    reset: () => setPage(1),
  }
}
