import { writable } from "svelte/store";
import type { InflightRequestEntry } from "../lib/types";

/** One point in a model's in-flight request count history. */
export interface InflightPoint {
  /** Millisecond timestamp at which the point was recorded. */
  t: number;
  /** In-flight request count for the model at time `t`. */
  count: number;
}

/** Maximum number of points kept per model. */
export const INFLIGHT_HISTORY_MAX_POINTS = 120;

// Per-model in-flight request count history, used to render small
// "recent activity" sparklines on the models overview. The api event
// handler records a point whenever a model's count changes, so a
// long-running request that emits frequent upserts without a count
// change does not spam the series. Each series is capped at
// INFLIGHT_HISTORY_MAX_POINTS points.
export const inflightHistory = writable<Record<string, InflightPoint[]>>({});

/** Count inflight request entries grouped by model. */
export function inflightCountsByModel(
  requests: Pick<InflightRequestEntry, "model">[],
): Record<string, number> {
  const counts: Record<string, number> = {};
  for (const request of requests) {
    counts[request.model] = (counts[request.model] ?? 0) + 1;
  }
  return counts;
}

/** Record the current per-model counts, appending a point only when a model's count changed. */
export function recordInflightCounts(counts: Record<string, number>): void {
  const now = Date.now();
  inflightHistory.update((history) => {
    let next: Record<string, InflightPoint[]> | null = null;
    for (const [model, count] of Object.entries(counts)) {
      const series = next?.[model] ?? history[model] ?? [];
      const last = series[series.length - 1];
      if (last && last.count === count) continue;
      const points =
        series.length >= INFLIGHT_HISTORY_MAX_POINTS
          ? [...series.slice(series.length - INFLIGHT_HISTORY_MAX_POINTS + 1)]
          : [...series];
      points.push({ t: now, count });
      if (!next) next = { ...history };
      next[model] = points;
    }
    return next ?? history;
  });
}
