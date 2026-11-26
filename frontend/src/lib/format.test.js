import { test } from "node:test";
import assert from "node:assert/strict";
import { fmtBytes, fmtCores, fmtValue, fmtAgo, pct, tone, bounds, linePath, parseRoute, appHref, shortRev, autoRollbackText, linkify, envOf, appGroup, groupApps } from "./format.js";

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
  assert.equal(fmtValue(null, "cores"), "–");
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

test("tone maps backup, node, Argo and rollout states", () => {
  assert.equal(tone("ok"), "good");
  assert.equal(tone("stale"), "warn");
  assert.equal(tone("failed"), "bad");
  assert.equal(tone("NotReady"), "bad");
  assert.equal(tone("progressing"), "info");
  assert.equal(tone("whatever"), "muted");
});

test("bounds and linePath scale points into the box", () => {
  const series = [{ label: "a", points: [{ t: 0, v: 0 }, { t: 10, v: 10 }] }];
  const b = bounds(series);
  assert.deepEqual(b, { tMin: 0, tMax: 10, vMax: 11 });
  assert.equal(linePath(series[0].points, b, 100, 110), "M0.0,110.0 L100.0,10.0");
  assert.equal(bounds([{ label: "x", points: [] }]), null);
});

test("parseRoute and appHref round-trip", () => {
  assert.deepEqual(parseRoute(""), { view: "overview" });
  assert.deepEqual(parseRoute("#backups"), { view: "backups" });
  assert.deepEqual(parseRoute("#nonsense"), { view: "overview" });
  assert.deepEqual(parseRoute(appHref("shop-testing", "my app")), { view: "app", namespace: "shop-testing", app: "my app" });
});

test("shortRev", () => {
  assert.equal(shortRev("abcdef1234"), "abcdef1");
  assert.equal(shortRev(""), "–");
});

test("autoRollbackText explains each state", () => {
  const r = (auto, allowed, reason = "") => ({ rollback: { auto, allowed, reason, grace: "5m0s" } });
  assert.match(autoRollbackText(r(true, true), false), /off for this clusterdeck install/);
  assert.match(autoRollbackText(r(false, true), true), /Annotate/);
  assert.match(autoRollbackText(r(true, false, "self-heals"), true), /cannot act, for the reason above/);
  assert.match(autoRollbackText(r(true, true), true), /after 5m0s/);
  const pr = { rollback: { auto: true, allowed: true, mode: "pull-request", grace: "5m0s" } };
  assert.match(autoRollbackText(pr, true), /opens a rollback pull request after 5m0s/);
});

test("linkify splits out https links only", () => {
  assert.deepEqual(linkify("opened pull request #9: https://github.com/o/r/pull/9"), [
    { text: "opened pull request #9: " },
    { text: "https://github.com/o/r/pull/9", href: "https://github.com/o/r/pull/9" },
  ]);
  assert.deepEqual(linkify("no links, javascript:alert(1)"), [{ text: "no links, javascript:alert(1)" }]);
  assert.deepEqual(linkify(""), []);
});

test("envOf reads the namespace suffix", () => {
  assert.equal(envOf("nextexplorer-testing"), "testing");
  assert.equal(envOf("nextexplorer"), "");
  assert.equal(envOf("cert-manager"), "");
});

test("appGroup prefers part-of, else the namespace without its environment", () => {
  assert.equal(appGroup({ namespace: "nextexplorer-testing", name: "nextexplorer" }), "nextexplorer");
  assert.equal(appGroup({ namespace: "demo", name: "podinfo", partOf: "gitops-demo" }), "gitops-demo");
  assert.equal(appGroup({ namespace: "argocd", name: "argocd-redis" }), "argocd");
});

test("groupApps puts every environment of an app together, base first", () => {
  const apps = [
    { namespace: "sure-testing", name: "sure" },
    { namespace: "nextexplorer-testing", name: "nextexplorer" },
    { namespace: "nextexplorer", name: "nextexplorer" },
    { namespace: "sure", name: "sure" },
  ];
  const g = groupApps(apps, "app");
  assert.deepEqual(g.map((x) => x.key), ["nextexplorer", "sure"]);
  assert.deepEqual(g[0].apps.map((a) => a.namespace), ["nextexplorer", "nextexplorer-testing"]);
  assert.equal(groupApps(apps, "namespace").length, 4);
  assert.equal(groupApps(apps, "none")[0].apps.length, 4);
});
