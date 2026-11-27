import { test } from "node:test";
import assert from "node:assert/strict";
import { logParams, rangeError, fmtLogTime, toggleLevel, highlight, summary, toLocalInput, fromLocalInput } from "./logs.js";

const now = Date.parse("2026-11-26T12:00:30.500Z");

test("a preset range becomes a since time the server filters on", () => {
  const p = new URLSearchParams(logParams({ container: "web", tail: 200, range: "1h", levels: ["error", "warn"], q: "  timeout " }, now));
  assert.equal(p.get("container"), "web");
  assert.equal(p.get("tail"), "200");
  assert.equal(p.get("since"), "2026-11-26T11:00:30Z");
  assert.equal(p.get("until"), null);
  assert.equal(p.get("level"), "error,warn");
  assert.equal(p.get("q"), "timeout");
  assert.equal(p.get("previous"), null);
});

test("all time and no filters send only the tail", () => {
  assert.equal(logParams({ range: "all", levels: [], q: "" }, now), "tail=500");
});

test("a custom range sends both ends, and previous asks for the last container", () => {
  const from = toLocalInput(Date.parse("2026-11-25T08:00:00Z"));
  const to = toLocalInput(Date.parse("2026-11-25T09:30:00Z"));
  const p = new URLSearchParams(logParams({ range: "custom", from, to, previous: true }, now));
  assert.equal(p.get("since"), "2026-11-25T08:00:00Z");
  assert.equal(p.get("until"), "2026-11-25T09:30:00Z");
  assert.equal(p.get("previous"), "true");
});

test("a custom range needs a sensible window", () => {
  assert.equal(rangeError({ range: "1h" }), "");
  assert.match(rangeError({ range: "custom", from: "", to: "" }), /Pick/);
  assert.match(rangeError({ range: "custom", from: "2026-11-25T10:00", to: "2026-11-25T09:00" }), /after/);
  assert.equal(rangeError({ range: "custom", from: "2026-11-25T10:00", to: "" }), "");
  assert.equal(fromLocalInput("nonsense"), null);
});

test("log times show the date only when it is not today", () => {
  const local = new Date(now);
  const today = new Date(local.getFullYear(), local.getMonth(), local.getDate(), 9, 5, 7, 42).toISOString();
  const earlier = new Date(local.getFullYear(), local.getMonth(), local.getDate() - 2, 23, 0, 0, 0).toISOString();
  assert.equal(fmtLogTime(today, now), "09:05:07.042");
  assert.match(fmtLogTime(earlier, now), /^\d{2}-\d{2} 23:00:00\.000$/);
  assert.equal(fmtLogTime(undefined, now), "");
});

test("severity toggles keep a stable order", () => {
  assert.deepEqual(toggleLevel([], "warn"), ["warn"]);
  assert.deepEqual(toggleLevel(["warn"], "error"), ["error", "warn"]);
  assert.deepEqual(toggleLevel(["error", "warn"], "error"), ["warn"]);
});

test("highlight marks every case-insensitive match", () => {
  assert.deepEqual(highlight("Timeout then timeout", "timeout"), [
    { text: "Timeout", hit: true },
    { text: " then ", hit: false },
    { text: "timeout", hit: true },
  ]);
  assert.deepEqual(highlight("abc", ""), [{ text: "abc", hit: false }]);
});

test("summary explains what the server left out", () => {
  assert.equal(summary({ lines: [1, 2], matched: 2, scanned: 2, truncated: false }, 500), "2 lines");
  assert.equal(summary({ lines: [1], matched: 1, scanned: 40, truncated: false }, 500), "1 line · 1 of 40 scanned lines match");
  assert.equal(summary({ lines: [1, 2], matched: 9, scanned: 9, truncated: true }, 2), "2 lines · newest 2 shown");
  assert.equal(summary({ lines: [1], matched: 1, scanned: 1, truncated: true }, 500), "1 line · older lines cut off");
});
