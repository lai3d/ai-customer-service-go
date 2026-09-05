import { useCallback, useEffect, useState } from 'react'
import { ApiError } from '../api/client'

/** Every page does the same three things with a request, and doing them by hand in four
 *  places is how one of them ends up rendering an empty table where it should be showing
 *  an error. */
export function useLoad<T>(load: () => Promise<T>, deps: unknown[]) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)

  const run = useCallback(async () => {
    setLoading(true)
    try {
      setData(await load())
      setError('')
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err))
    } finally {
      setLoading(false)
    }
    // load is rebuilt on every render; the caller's deps are the real dependency list.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)

  useEffect(() => { void run() }, [run])
  return { data, error, loading, reload: run }
}

export const when = (iso: string) => new Date(iso).toLocaleString()
