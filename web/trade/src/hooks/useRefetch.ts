import { useCallback, useEffect, useRef, useState } from 'react'
import { describeError } from '../api/client'

// useResource fetches once per change of `key`, exposes a `bump` that
// refetches after a short debounce (private-stream frames arrive in bursts:
// one order can produce accepted, filled and balance frames within a
// millisecond), and keeps the last good value while a refetch is running.
export function useResource<T>(load: () => Promise<T>, key: string): { data: T | null; error: string | null; bump: () => void; reload: () => void } {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [version, setVersion] = useState(0)
  const loader = useRef(load)
  loader.current = load
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    let cancelled = false
    loader
      .current()
      .then((v) => {
        if (!cancelled) {
          setData(v)
          setError(null)
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(describeError(err))
      })
    return () => {
      cancelled = true
    }
  }, [key, version])

  const reload = useCallback(() => setVersion((v) => v + 1), [])
  const bump = useCallback(() => {
    if (timer.current) clearTimeout(timer.current)
    timer.current = setTimeout(() => {
      timer.current = null
      setVersion((v) => v + 1)
    }, 150)
  }, [])
  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current)
  }, [])
  return { data, error, bump, reload }
}
