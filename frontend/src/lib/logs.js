export const LEVELS = [
  ["error", "Error"],
  ["warn", "Warn"],
  ["info", "Info"],
  ["debug", "Debug"],
  ["unknown", "Other"],
];

export const RANGES = [
  ["15m", "15 min", 15 * 60e3],
  ["1h", "1 hour", 3600e3],
  ["6h", "6 hours", 6 * 3600e3],
  ["24h", "24 hours", 24 * 3600e3],
  ["7d", "7 days", 7 * 24 * 3600e3],
  ["all", "All", 0],
  ["custom", "Custom", 0],
];

export const TAILS = [200, 500, 1000, 5000];

export function logParams(f, now = Date.now()) {
  const p = new URLSearchParams();
  if (f.container) p.set("container", f.container);
  p.set("tail", String(f.tail || 500));
  const preset = RANGES.find(([k]) => k === f.range);
  if (preset && preset[2]) p.set("since", isoSeconds(now - preset[2]));
  if (f.range === "custom") {
    const since = fromLocalInput(f.from);
    const until = fromLocalInput(f.to);
    if (since != null) p.set("since", isoSeconds(since));
    if (until != null) p.set("until", isoSeconds(until));
  }
  if (f.levels?.length) p.set("level", f.levels.join(","));
  if (f.q?.trim()) p.set("q", f.q.trim());
  if (f.previous) p.set("previous", "true");
  return p.toString();
}

export function rangeError(f) {
  if (f.range !== "custom") return "";
  const since = fromLocalInput(f.from);
  const until = fromLocalInput(f.to);
  if (since == null && until == null) return "Pick a start or an end time.";
  if (since != null && until != null && until <= since) return "The end must be after the start.";
  return "";
}

function isoSeconds(ms) {
  return new Date(Math.floor(ms / 1000) * 1000).toISOString().replace(".000Z", "Z");
}

export function toLocalInput(ms) {
  const d = new Date(ms);
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function fromLocalInput(s) {
  if (!s) return null;
  const t = new Date(s).getTime();
  return Number.isFinite(t) ? t : null;
}

export function fmtLogTime(iso, now = Date.now()) {
  if (!iso) return "";
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return "";
  const pad = (n, w = 2) => String(n).padStart(w, "0");
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
  const today = new Date(now);
  if (d.toDateString() === today.toDateString()) return time;
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}`;
}

export function toggleLevel(levels, level) {
  const on = new Set(levels);
  if (on.has(level)) on.delete(level);
  else on.add(level);
  return LEVELS.map(([k]) => k).filter((k) => on.has(k));
}

export function highlight(text, q) {
  const needle = q?.trim().toLowerCase();
  if (!needle) return [{ text, hit: false }];
  const out = [];
  const lower = text.toLowerCase();
  let i = 0;
  for (let j = lower.indexOf(needle); j !== -1; j = lower.indexOf(needle, i)) {
    if (j > i) out.push({ text: text.slice(i, j), hit: false });
    out.push({ text: text.slice(j, j + needle.length), hit: true });
    i = j + needle.length;
  }
  if (i < text.length) out.push({ text: text.slice(i), hit: false });
  return out;
}

export function summary(data, tail) {
  if (!data) return "";
  const shown = data.lines.length;
  const parts = [`${shown} ${shown === 1 ? "line" : "lines"}`];
  if (data.matched !== data.scanned) parts.push(`${data.matched} of ${data.scanned} scanned lines match`);
  if (data.matched > shown) parts.push(`newest ${tail} shown`);
  else if (data.truncated) parts.push("older lines cut off");
  return parts.join(" · ");
}
