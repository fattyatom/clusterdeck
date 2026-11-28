export function fmtDuration(seconds) {
  if (seconds == null || !Number.isFinite(seconds)) return "–";
  const s = Math.max(0, Math.round(seconds));
  if (s === 0) return "done";
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  return m ? `${h}h ${m}m` : `${h}h`;
}

const WAIT_TONES = { soak: "info", watch: "info", sync: "info", merge: "good", CI: "warn", health: "warn", human: "warn" };

export function waitTone(row) {
  if (row.health === "failure" || row.ci === "failure") return "bad";
  return WAIT_TONES[row.waiting_on] ?? "muted";
}

export function waitText(row) {
  switch (row.waiting_on) {
    case "soak":
      return `soak · ${fmtDuration(row.soak_remaining_s)} left`;
    case "watch":
      return `watch · ${fmtDuration(row.watch_remaining_s)} left`;
    case "merge":
      return "merge";
    default:
      return row.waiting_on || "–";
  }
}

export function change(row) {
  if (row.from && row.to) return `${row.from} → ${row.to}`;
  return row.to || row.from || "–";
}

const STATE_TONES = { success: "good", pending: "info", failure: "bad", unknown: "muted", merged: "muted" };

export function stateTone(state) {
  return STATE_TONES[state] ?? "muted";
}

export function rowKey(row) {
  return row.pr ? `pr-${row.pr}` : `${row.kind}-${row.app}-${row.env}`;
}
