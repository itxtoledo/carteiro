import { useCallback, useEffect, useRef, useState } from "react";

/**
 * Polls `loader` every `intervalMs` (0 disables polling) and whenever
 * `key` or the returned `reload` changes. Safe against overlapping runs.
 */
export function usePolling<T>(key: string, loader: () => Promise<T>, intervalMs: number) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);
  const inFlight = useRef(false);

  const run = useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    try {
      const d = await loader();
      setData(d);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      inFlight.current = false;
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  useEffect(() => {
    run();
    if (intervalMs > 0) {
      const id = window.setInterval(run, intervalMs);
      return () => window.clearInterval(id);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [run, tick]);

  return { data, error, loading, reload: () => setTick((n) => n + 1) };
}
