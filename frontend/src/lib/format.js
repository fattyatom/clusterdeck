const UNITS = ["B", "KiB", "MiB", "GiB", "TiB"];

export function fmtBytes(n) {
  if (n == null || !Number.isFinite(n)) return "–";
  let i = 0;
  let v = Math.abs(n);
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  const s = v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1);
  return `${n < 0 ? "-" : ""}${s} ${UNITS[i]}`;
}

export function fmtCores(milli) {
  if (milli == null || !Number.isFinite(milli)) return "–";
  if (milli < 1000) return `${Math.round(milli)}m`;
  return `${(milli / 1000).toFixed(milli < 10000 ? 2 : 1)} cores`;
}

export function fmtValue(v, unit) {
  if (unit === "bytes") return fmtBytes(v);
  if (unit === "cores") return fmtCores(v == null ? v : v * 1000);
  if (v == null || !Number.isFinite(v)) return "–";
  return Number.isInteger(v) ? String(v) : v.toFixed(2);
}

export function fmtAgo(iso, now = Date.now()) {
  if (!iso) return "never";
  const t = typeof iso === "number" ? iso : Date.parse(iso);
  if (!Number.isFinite(t)) return "–";
  const s = Math.round((now - t) / 1000);
  const a = Math.abs(s);
  let out;
  if (a < 60) out = `${a}s`;
  else if (a < 3600) out = `${Math.floor(a / 60)}m`;
  else if (a < 86400) out = `${Math.floor(a / 3600)}h`;
  else out = `${Math.floor(a / 86400)}d`;
  return s < 0 ? `in ${out}` : `${out} ago`;
}

export function pct(used, total) {
  if (used == null || !total) return null;
  return Math.max(0, Math.min(100, (used / total) * 100));
}

const TONES = {
  ok: "good",
  succeeded: "good",
  Healthy: "good",
  Synced: "good",
  Ready: "good",
  complete: "good",
  done: "good",
  running: "info",
  progressing: "info",
  Progressing: "info",
  active: "info",
  pending: "muted",
  paused: "muted",
  suspended: "muted",
  unknown: "muted",
  stale: "warn",
  OutOfSync: "warn",
  cordoned: "warn",
  failed: "bad",
  never: "bad",
  Degraded: "bad",
  Missing: "bad",
  NotReady: "bad",
};

export function tone(status) {
  return TONES[status] ?? "muted";
}

export function shortRev(rev) {
  return rev ? rev.slice(0, 7) : "–";
}

export function bounds(series) {
  let tMin = Infinity;
  let tMax = -Infinity;
  let vMax = 0;
  for (const s of series) {
    for (const p of s.points) {
      if (p.t < tMin) tMin = p.t;
      if (p.t > tMax) tMax = p.t;
      if (p.v > vMax) vMax = p.v;
    }
  }
  if (!Number.isFinite(tMin)) return null;
  if (tMax === tMin) tMax = tMin + 1;
  return { tMin, tMax, vMax: vMax > 0 ? vMax * 1.1 : 1 };
}

export function linePath(points, b, width, height) {
  if (!points.length) return "";
  return points
    .map((p, i) => {
      const x = ((p.t - b.tMin) / (b.tMax - b.tMin)) * width;
      const y = height - (p.v / b.vMax) * height;
      return `${i ? "L" : "M"}${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
}

export function parseRoute(hash) {
  const parts = (hash || "").replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
  const view = parts[0] || "overview";
  if (view === "apps" && parts.length === 3) return { view: "app", namespace: parts[1], app: parts[2] };
  if (["overview", "apps", "backups", "load"].includes(view)) return { view };
  return { view: "overview" };
}

export function appHref(namespace, app) {
  return `#apps/${encodeURIComponent(namespace)}/${encodeURIComponent(app)}`;
}

export function linkify(text) {
  const parts = [];
  const re = /https:\/\/[^\s<>"']+/g;
  let last = 0;
  for (const m of String(text ?? "").matchAll(re)) {
    if (m.index > last) parts.push({ text: text.slice(last, m.index) });
    parts.push({ text: m[0], href: m[0] });
    last = m.index + m[0].length;
  }
  if (last < String(text ?? "").length) parts.push({ text: String(text).slice(last) });
  return parts;
}

export function autoRollbackText(rollout, enabledInstall) {
  if (!enabledInstall) return "Auto-rollback is off for this clusterdeck install.";
  if (!rollout.rollback.auto) return 'Auto-rollback is off. Annotate the Deployment with clusterdeck.io/auto-rollback: "true" to turn it on.';
  if (!rollout.rollback.allowed) return "Auto-rollback is on, but cannot act, for the reason above.";
  if (rollout.rollback.mode === "pull-request") return `Auto-rollback is on: a failing revision opens a rollback pull request after ${rollout.rollback.grace}, or as soon as its progress deadline passes. Merging it is up to you.`;
  return `Auto-rollback is on: a failing revision is rolled back after ${rollout.rollback.grace}, or as soon as its progress deadline passes.`;
}

const ENV_SUFFIX = /-(testing|test|staging|stage|dev|qa|uat|preview|canary|sandbox|prod|production)$/;

// envOf names the environment a namespace belongs to, read from its suffix ("nextexplorer-testing" → "testing").
export function envOf(namespace) {
  return String(namespace ?? "").match(ENV_SUFFIX)?.[1] ?? "";
}

// appGroup is the application an app card belongs to across environments: its
// app.kubernetes.io/part-of label, or else its namespace without the environment suffix.
export function appGroup(app) {
  return app.partOf || String(app.namespace ?? "").replace(ENV_SUFFIX, "");
}

// groupApps buckets apps by "app" (application across environments), "namespace", or "none".
// Groups are sorted by name; within one, the unsuffixed environment comes first.
export function groupApps(apps, by) {
  if (by === "none") return [{ key: "", apps }];
  const keyOf = by === "namespace" ? (a) => a.namespace : appGroup;
  const groups = new Map();
  for (const a of apps) {
    const k = keyOf(a);
    if (!groups.has(k)) groups.set(k, []);
    groups.get(k).push(a);
  }
  const rank = (a) => [envOf(a.namespace), a.namespace, a.name];
  return [...groups.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([key, list]) => ({
      key,
      apps: list.slice().sort((x, y) => {
        const [ex, nx, ax] = rank(x);
        const [ey, ny, ay] = rank(y);
        return ex.localeCompare(ey) || nx.localeCompare(ny) || ax.localeCompare(ay);
      }),
    }));
}
