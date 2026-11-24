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
  if (unit === "cores") return fmtCores(v * 1000);
  if (v == null || !Number.isFinite(v)) return "–";
  return Number.isInteger(v) ? String(v) : v.toFixed(2);
}

export function fmtAgo(iso, now = Date.now()) {
  if (!iso) return "never";
  const t = typeof iso === "number" ? iso : Date.parse(iso);
  if (!Number.isFinite(t)) return "–";
  const s = Math.round((now - t) / 1000);
  const future = s < 0;
  const a = Math.abs(s);
  let out;
  if (a < 60) out = `${a}s`;
  else if (a < 3600) out = `${Math.floor(a / 60)}m`;
  else if (a < 86400) out = `${Math.floor(a / 3600)}h`;
  else out = `${Math.floor(a / 86400)}d`;
  return future ? `in ${out}` : `${out} ago`;
}

export function pct(used, total) {
  if (used == null || !total) return null;
  return Math.max(0, Math.min(100, (used / total) * 100));
}

const TONES = {
  ok: "good",
  Ready: "good",
  NotReady: "bad",
  cordoned: "warn",
  succeeded: "good",
  Healthy: "good",
  Synced: "good",
  running: "info",
  pending: "muted",
  suspended: "muted",
  unknown: "muted",
  stale: "warn",
  OutOfSync: "warn",
  Progressing: "info",
  failed: "bad",
  never: "bad",
  Degraded: "bad",
  Missing: "bad",
};

export function tone(status) {
  return TONES[status] ?? "muted";
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

export function flatChildren(children) {
  return children.flat(Infinity).filter((c) => c != null && c !== false);
}

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of flatChildren(children)) {
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}
