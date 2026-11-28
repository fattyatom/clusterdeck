import { test } from "node:test";
import assert from "node:assert/strict";
import { fmtDuration, waitTone, waitText, change, stateTone, rowKey } from "./inflight.js";

test("fmtDuration rounds to the largest useful unit", () => {
  assert.equal(fmtDuration(0), "done");
  assert.equal(fmtDuration(-5), "done");
  assert.equal(fmtDuration(42), "42s");
  assert.equal(fmtDuration(2400), "40m");
  assert.equal(fmtDuration(3600), "1h");
  assert.equal(fmtDuration(5400), "1h 30m");
  assert.equal(fmtDuration(null), "–");
});

test("waitText names the remaining window", () => {
  assert.equal(waitText({ waiting_on: "soak", soak_remaining_s: 600 }), "soak · 10m left");
  assert.equal(waitText({ waiting_on: "watch", watch_remaining_s: 2400 }), "watch · 40m left");
  assert.equal(waitText({ waiting_on: "CI" }), "CI");
  assert.equal(waitText({}), "–");
});

test("a failing gate turns the waiting badge red", () => {
  assert.equal(waitTone({ waiting_on: "health", health: "failure" }), "bad");
  assert.equal(waitTone({ waiting_on: "CI", ci: "failure" }), "bad");
  assert.equal(waitTone({ waiting_on: "health", health: "unknown" }), "warn");
  assert.equal(waitTone({ waiting_on: "merge" }), "good");
  assert.equal(waitTone({ waiting_on: "watch" }), "info");
});

test("change shows from → to when both are known", () => {
  assert.equal(change({ from: "1.2.3", to: "1.2.4" }), "1.2.3 → 1.2.4");
  assert.equal(change({ to: "abc1234" }), "abc1234");
  assert.equal(change({}), "–");
});

test("stateTone maps gate states", () => {
  assert.equal(stateTone("success"), "good");
  assert.equal(stateTone("failure"), "bad");
  assert.equal(stateTone("whatever"), "muted");
});

test("rowKey is unique per pull request or per app sync", () => {
  assert.equal(rowKey({ pr: 7 }), "pr-7");
  assert.equal(rowKey({ kind: "sync", app: "sure", env: "testing" }), "sync-sure-testing");
});
