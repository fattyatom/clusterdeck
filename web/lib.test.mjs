import { test } from "node:test";
import assert from "node:assert/strict";
import { fmtBytes, fmtCores, fmtValue, fmtAgo, pct, tone, bounds, linePath, flatChildren } from "./lib.js";

test("fmtBytes picks a binary unit", () => {
  assert.equal(fmtBytes(512), "512 B");
  assert.equal(fmtBytes(1536), "1.5 KiB");
  assert.equal(fmtBytes(8 * 1024 ** 3), "8.0 GiB");
  assert.equal(fmtBytes(null), "–");
});

test("fmtCores switches from millicores to cores", () => {
  assert.equal(fmtCores(250), "250m");
  assert.equal(fmtCores(1500), "1.50 cores");
  assert.equal(fmtCores(16000), "16.0 cores");
});

test("fmtValue follows the panel unit", () => {
  assert.equal(fmtValue(0.5, "cores"), "500m");
  assert.equal(fmtValue(2048, "bytes"), "2.0 KiB");
  assert.equal(fmtValue(3, "count"), "3");
});

test("fmtAgo covers past, future and missing", () => {
  const now = Date.parse("2026-01-01T12:00:00Z");
  assert.equal(fmtAgo("2026-01-01T11:59:30Z", now), "30s ago");
  assert.equal(fmtAgo("2026-01-01T10:00:00Z", now), "2h ago");
  assert.equal(fmtAgo("2026-01-01T12:05:00Z", now), "in 5m");
  assert.equal(fmtAgo(null, now), "never");
});

test("pct clamps and tolerates missing data", () => {
  assert.equal(pct(50, 200), 25);
  assert.equal(pct(300, 200), 100);
  assert.equal(pct(null, 200), null);
  assert.equal(pct(1, 0), null);
});

test("tone maps backup and Argo states", () => {
  assert.equal(tone("ok"), "good");
  assert.equal(tone("stale"), "warn");
  assert.equal(tone("failed"), "bad");
  assert.equal(tone("Degraded"), "bad");
  assert.equal(tone("NotReady"), "bad");
  assert.equal(tone("whatever"), "muted");
});

test("bounds and linePath scale points into the box", () => {
  const series = [{ label: "a", points: [{ t: 0, v: 0 }, { t: 10, v: 10 }] }];
  const b = bounds(series);
  assert.deepEqual(b, { tMin: 0, tMax: 10, vMax: 11 });
  assert.equal(linePath(series[0].points, b, 100, 110), "M0.0,110.0 L100.0,10.0");
  assert.equal(bounds([{ label: "x", points: [] }]), null);
});

test("flatChildren flattens any depth and drops empty children", () => {
  assert.deepEqual(flatChildren(["a", [["b", null], [false, ["c"]]], undefined, 0]), ["a", "b", "c", 0]);
});
