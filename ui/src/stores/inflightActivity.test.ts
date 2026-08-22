import { get } from "svelte/store";
import { afterEach, describe, expect, it } from "vitest";
import {
  INFLIGHT_HISTORY_MAX_POINTS,
  inflightCountsByModel,
  inflightHistory,
  recordInflightCounts,
} from "./inflightActivity";

afterEach(() => {
  inflightHistory.set({});
});

describe("inflightCountsByModel", () => {
  it("counts entries per model", () => {
    expect(
      inflightCountsByModel([{ model: "a" }, { model: "b" }, { model: "a" }]),
    ).toEqual({ a: 2, b: 1 });
  });

  it("returns an empty map when there are no requests", () => {
    expect(inflightCountsByModel([])).toEqual({});
  });
});

describe("recordInflightCounts", () => {
  it("appends a point when a model count changes", () => {
    recordInflightCounts({ a: 1 });
    recordInflightCounts({ a: 2 });
    expect(get(inflightHistory).a.map((p) => p.count)).toEqual([1, 2]);
  });

  it("skips points when the count is unchanged", () => {
    recordInflightCounts({ a: 1 });
    recordInflightCounts({ a: 1 });
    recordInflightCounts({ a: 1 });
    expect(get(inflightHistory).a).toHaveLength(1);
  });

  it("records drops to zero", () => {
    recordInflightCounts({ a: 2 });
    recordInflightCounts({ a: 0 });
    expect(get(inflightHistory).a.map((p) => p.count)).toEqual([2, 0]);
  });

  it("tracks each model independently", () => {
    recordInflightCounts({ a: 1, b: 3 });
    recordInflightCounts({ a: 0 });
    expect(get(inflightHistory).a.map((p) => p.count)).toEqual([1, 0]);
    expect(get(inflightHistory).b.map((p) => p.count)).toEqual([3]);
  });

  it("caps each series at the maximum number of points", () => {
    const total = INFLIGHT_HISTORY_MAX_POINTS + 10;
    for (let count = 1; count <= total; count++) {
      recordInflightCounts({ a: count });
    }
    const series = get(inflightHistory).a;
    expect(series).toHaveLength(INFLIGHT_HISTORY_MAX_POINTS);
    expect(series[0].count).toBe(total - INFLIGHT_HISTORY_MAX_POINTS + 1);
    expect(series[series.length - 1].count).toBe(total);
  });

  it("does not mutate previously emitted values", () => {
    recordInflightCounts({ a: 1 });
    const before = get(inflightHistory).a;
    recordInflightCounts({ a: 2 });
    const after = get(inflightHistory).a;
    expect(before).toHaveLength(1);
    expect(after).toHaveLength(2);
  });
});
