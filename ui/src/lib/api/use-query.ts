"use client";

import { useCallback, useEffect, useState } from "react";
import { usePlatform } from "@/components/platform-provider";
import { ConnectError } from "@connectrpc/connect";

export function errorMessage(error: unknown): string {
  if (error instanceof ConnectError) return error.rawMessage || error.message;
  return error instanceof Error ? error.message : "The request could not be completed.";
}

/** Call with a memoized loader; the page is remounted when tenant/auth scope changes. */
export function useAsyncQuery<T>(load: (signal: AbortSignal) => Promise<T>, options: { pollMs?: number; observeOperation?: boolean; isComplete?: (data: T) => boolean } = {}) {
  const [state, setState] = useState<{ loader?: typeof load; data?: T; error: string; loading: boolean; refreshing: boolean }>({ error: "", loading: true, refreshing: false });
  const [revision, setRevision] = useState(0);
  const refresh = useCallback(() => setRevision((value) => value + 1), []);
  const { liveUpdates } = usePlatform();
  const pollMs = liveUpdates || options.observeOperation ? options.pollMs : undefined;
  const livePolling = liveUpdates && pollMs !== undefined;
  const isComplete = options.isComplete;
  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const started = Date.now();
    const run = async () => {
      let completed = false;
      setState((previous) => previous.loader === load ? { ...previous, refreshing: true } : { loader: load, error: "", loading: true, refreshing: true });
      try {
        const data = await load(controller.signal);
        completed = isComplete?.(data) ?? false;
        if (!controller.signal.aborted) setState({ loader: load, data, error: "", loading: false, refreshing: false });
      } catch (error) {
        if (!controller.signal.aborted) setState((previous) => ({ ...previous, loader: load, error: errorMessage(error), loading: false, refreshing: false }));
      }
      // Polling is bounded; leaving the page or changing scope cancels it immediately.
      // Completion ends submitted-operation tracking, but not an explicit Live updates subscription.
      if ((livePolling || !completed) && pollMs && pollMs >= 1000 && !controller.signal.aborted && Date.now() - started < 15 * 60_000) {
        timer = setTimeout(run, pollMs);
      }
    };
    void run();
    return () => { controller.abort(); if (timer) clearTimeout(timer); };
  }, [load, revision, pollMs, isComplete, livePolling]);
  const current = state.loader === load;
  return { data: current ? state.data : undefined, error: current ? state.error : "", loading: !current || state.loading, refreshing: current && state.refreshing, refresh };
}
