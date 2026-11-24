import { h, flatChildren, fmtBytes, fmtCores, fmtValue, fmtAgo, pct, tone, bounds, linePath } from "./lib.js";

const main = document.getElementById("main");
const dialog = document.getElementById("dialog");
let config = { actionsEnabled: false, panels: [], metricsProvider: "none" };
let range = "6h";

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { "Content-Type": "application/json", "X-Requested-With": "clusterdeck", ...(opts.headers || {}) },
  });
  const type = res.headers.get("Content-Type") || "";
  const body = type.includes("json") ? await res.json() : await res.text();
  if (!res.ok) throw new Error((body && body.error) || `HTTP ${res.status}`);
  return body;
}

const fill = (el, ...children) => el.replaceChildren(...flatChildren(children));

const badge = (status) => h("span", { class: `badge ${tone(status)}` }, status || "–");

function usageBar(used, total) {
  const p = pct(used, total);
  const span = h("span", { class: p > 90 ? "crit" : p > 75 ? "hot" : "" });
  span.style.width = `${p ?? 0}%`;
  return h("div", { class: "bar", role: "meter", "aria-valuenow": p == null ? null : Math.round(p), "aria-valuemin": 0, "aria-valuemax": 100 }, span);
}

function showText(title, text) {
  fill(dialog, 
    h("h3", {}, title),
    h("pre", {}, text || "(empty)"),
    h("div", { class: "actions" }, h("button", { onclick: () => dialog.close() }, "Close")),
  );
  dialog.showModal();
}

async function act(label, path, body) {
  try {
    const out = await api(path, { method: "POST", body: JSON.stringify(body || {}) });
    showText(label, out.result);
  } catch (e) {
    showText(`${label} failed`, e.message);
  }
}

async function renderOverview() {
  const ov = await api("/api/overview");
  const ready = ov.nodes.filter((n) => n.ready).length;
  const phases = ov.podPhases || {};
  const kpis = h(
    "div",
    { class: "kpis" },
    kpi(`${ready}/${ov.nodes.length}`, "nodes ready", ready < ov.nodes.length ? "bad" : ""),
    kpi(phases.Running || 0, "pods running"),
    kpi(phases.Pending || 0, "pods pending", phases.Pending ? "warn" : ""),
    kpi(phases.Failed || 0, "pods failed", phases.Failed ? "bad" : ""),
    kpi(ov.namespaces, "namespaces"),
  );
  const nodes = ov.nodes.map((n) =>
    h(
      "div",
      { class: "card" },
      h("h3", {}, n.name, badge(n.ready ? (n.unschedulable ? "cordoned" : "Ready") : "NotReady")),
      h("div", { class: "small muted" }, `${n.roles.join(", ") || "worker"} · ${n.version} · ${n.podCount} pods`),
      n.pressure.length ? h("div", { class: "small warn" }, n.pressure.join(", ")) : null,
      h("div", { class: "row" }, h("span", {}, "CPU"), h("span", { class: "muted" }, `${fmtCores(n.cpuUsageMilli)} / ${fmtCores(n.cpuAllocatableMilli)}`)),
      usageBar(n.cpuUsageMilli, n.cpuAllocatableMilli),
      h("div", { class: "row" }, h("span", {}, "Memory"), h("span", { class: "muted" }, `${fmtBytes(n.memUsageBytes)} / ${fmtBytes(n.memAllocatableBytes)}`)),
      usageBar(n.memUsageBytes, n.memAllocatableBytes),
    ),
  );
  const warnings = ov.warnings.length
    ? table(
        ["When", "Object", "Reason", "Message"],
        ov.warnings.map((e) => [fmtAgo(e.time), h("span", { class: "mono" }, `${e.namespace}/${e.object}`), e.reason, e.message]),
      )
    : h("p", { class: "muted" }, "No warning events.");
  fill(main, 
    h("h2", {}, "Cluster"),
    kpis,
    h("h2", {}, "Nodes"),
    ov.liveUsage ? null : h("p", { class: "muted small" }, "metrics-server is not answering, so live usage is not shown."),
    h("div", { class: "grid" }, nodes),
    h("h2", {}, "Recent warnings"),
    warnings,
  );
}

function kpi(value, label, cls = "") {
  return h("div", { class: "card kpi" }, h("div", { class: `v ${cls}` }, value), h("div", { class: "l" }, label));
}

function table(head, rows) {
  return h(
    "div",
    { class: "table-wrap" },
    h("table", {}, h("thead", {}, h("tr", {}, head.map((c) => h("th", {}, c)))), h("tbody", {}, rows.map((r) => h("tr", {}, r.map((c) => h("td", {}, c)))))),
  );
}

async function renderApps() {
  const apps = await api("/api/apps");
  if (!apps.length) {
    fill(main, h("p", { class: "muted" }, "No workloads found."));
    return;
  }
  const cards = apps.map((a) =>
    h(
      "div",
      { class: "card" },
      h("h3", {}, h("span", {}, a.name, " ", h("span", { class: "muted small" }, a.namespace)), badge(a.healthy ? "ok" : "failed")),
      a.argo.map((s) => h("div", { class: "small" }, "Argo CD ", h("span", { class: "mono" }, s.application), " ", badge(s.sync), " ", badge(s.health))),
      a.restarts ? h("div", { class: "small warn" }, `${a.restarts} container restarts`) : null,
      a.workloads.map((w) =>
        h(
          "div",
          { class: "workload" },
          h(
            "div",
            { class: "row" },
            h("span", { class: "mono" }, `${w.kind.toLowerCase()}/${w.name}`),
            h("span", { class: w.ready < w.desired ? "bad" : "muted" }, `${w.ready}/${w.desired}`),
          ),
          h("div", { class: "small muted mono" }, w.images.join(", ")),
          h(
            "ul",
            { class: "pods" },
            w.pods.map((p) =>
              h(
                "li",
                {},
                h("span", { class: `mono ${p.ready ? "" : "warn"}` }, p.name),
                h(
                  "span",
                  { class: "actions" },
                  p.restarts ? h("span", { class: "warn" }, `↻${p.restarts}`) : null,
                  h("button", { onclick: () => logs(a.namespace, p) }, "Logs"),
                ),
              ),
            ),
          ),
          config.actionsEnabled
            ? h(
                "div",
                { class: "actions" },
                h("button", { onclick: () => confirm(`Restart ${w.kind} ${a.namespace}/${w.name}?`) && act("Restart", `/api/workloads/${a.namespace}/${w.kind.toLowerCase()}/${w.name}/restart`) }, "Rolling restart"),
              )
            : null,
        ),
      ),
    ),
  );
  fill(main, h("h2", {}, `Apps (${apps.length})`), h("div", { class: "grid" }, cards));
}

async function logs(ns, pod) {
  const container = pod.containers.length > 1 ? prompt(`Container (${pod.containers.join(", ")})`, pod.containers[0]) : pod.containers[0];
  if (container == null) return;
  try {
    const text = await api(`/api/pods/${ns}/${pod.name}/logs?tail=300&container=${encodeURIComponent(container)}`);
    showText(`${ns}/${pod.name} · ${container}`, text);
  } catch (e) {
    showText("Logs failed", e.message);
  }
}

async function renderBackups() {
  const [jobs, templates] = await Promise.all([api("/api/backups"), api("/api/restores")]);
  const rows = jobs.map((j) => [
    h("div", {}, h("div", { class: "mono" }, `${j.namespace}/${j.name}`), j.description ? h("div", { class: "small muted" }, j.description) : null),
    j.role === "backup" ? "backup" : "restore check",
    h("span", { class: "mono" }, j.schedule, j.timeZone ? ` (${j.timeZone})` : ""),
    badge(j.health),
    fmtAgo(j.lastSuccess),
    j.nextRun ? fmtAgo(j.nextRun) : "–",
    h("span", { class: "dots", title: "newest first" }, j.recent.map((r) => h("span", { class: `dot ${tone(r.state)}`, title: `${r.name}: ${r.state}${r.manual ? " (manual)" : ""}` }))),
    config.actionsEnabled ? h("button", { onclick: () => confirm(`Run ${j.namespace}/${j.name} now?`) && act("Run now", `/api/cronjobs/${j.namespace}/${j.name}/run`) }, "Run now") : "",
  ]);
  const restoreRows = templates.map((t) => [
    h("div", {}, h("div", { class: "mono" }, `${t.namespace}/${t.name}`), t.description ? h("div", { class: "small muted" }, t.description) : null, t.error ? h("div", { class: "small err" }, t.error) : null),
    t.app || "–",
    t.params.map((p) => p.name).join(", ") || "–",
    config.actionsEnabled && !t.error ? h("button", { class: "danger", onclick: () => restoreDialog(t) }, "Restore…") : "",
  ]);
  fill(main, 
    h("h2", {}, "Backups & restore checks"),
    jobs.length ? table(["Job", "Kind", "Schedule", "Health", "Last success", "Next run", "Recent", ""], rows) : h("p", { class: "muted" }, "No CronJobs carry the backup or restore-check label. See the README for the labels."),
    h("h2", {}, "Restore"),
    templates.length ? table(["Template", "App", "Parameters", ""], restoreRows) : h("p", { class: "muted" }, "No restore templates found."),
  );
}

function restoreDialog(t) {
  const target = `${t.namespace}/${t.name}`;
  const inputs = t.params.map((p) => [p.name, h("input", { name: p.name, value: p.default, placeholder: p.default || "(template default)", autocomplete: "off" })]);
  const confirmInput = h("input", { autocomplete: "off", placeholder: target });
  const run = h("button", { class: "danger", disabled: true }, "Run restore");
  confirmInput.addEventListener("input", () => (run.disabled = confirmInput.value !== target));
  run.addEventListener("click", () => {
    const params = {};
    for (const [name, input] of inputs) if (input.value !== "") params[name] = input.value;
    dialog.close();
    act("Restore", `/api/restores/${t.namespace}/${t.name}/run`, { confirm: confirmInput.value, params });
  });
  fill(dialog, 
    h("h3", {}, `Restore ${target}`),
    h("p", { class: "warn" }, "A restore usually overwrites live data. Read the template's description before running it."),
    t.description ? h("p", { class: "muted" }, t.description) : null,
    h("div", {}, inputs.map(([name, input]) => [h("label", {}, name), input])),
    h("label", {}, `Type ${target} to confirm`),
    confirmInput,
    h("div", { class: "actions" }, run, h("button", { onclick: () => dialog.close() }, "Cancel")),
  );
  dialog.showModal();
}

async function renderLoad() {
  if (config.metricsProvider === "none") {
    fill(main, h("div", { class: "notice" }, "No metrics backend is configured. Set METRICS_PROVIDER to signoz or prometheus for history; the Overview tab still shows live usage from metrics-server."));
    return;
  }
  const picker = h(
    "div",
    { class: "segmented", role: "group", "aria-label": "Time range" },
    ["1h", "6h", "24h", "7d"].map((r) => h("button", { class: r === range ? "on" : "", onclick: () => { range = r; route(); } }, r)),
  );
  const grid = h("div", { class: "grid" });
  fill(main, h("h2", {}, `Load · ${config.metricsProvider} `, picker), grid);
  for (const panel of config.panels) {
    const card = h("div", { class: "card chart" }, h("h3", {}, panel.title), h("div", { class: "muted small" }, "loading…"));
    grid.append(card);
    api(`/api/metrics/${panel.id}?range=${range}`)
      .then((series) => fill(card, h("h3", {}, panel.title), chart(series, panel.unit)))
      .catch((e) => fill(card, h("h3", {}, panel.title), h("div", { class: "err small" }, e.message)));
  }
}

const SVG = "http://www.w3.org/2000/svg";
function chart(series, unit) {
  const b = bounds(series);
  if (!b) return h("div", { class: "muted small" }, "No data in this range.");
  const W = 300;
  const H = 150;
  const svg = document.createElementNS(SVG, "svg");
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("role", "img");
  const shown = series.slice(0, 6);
  shown.forEach((s, i) => {
    const path = document.createElementNS(SVG, "path");
    path.setAttribute("d", linePath(s.points, b, W, H));
    path.setAttribute("fill", "none");
    path.setAttribute("stroke", "currentColor");
    path.setAttribute("stroke-width", "1.5");
    path.setAttribute("vector-effect", "non-scaling-stroke");
    path.setAttribute("class", `c${i + 1}`);
    svg.append(path);
  });
  const latest = (s) => (s.points.length ? s.points[s.points.length - 1].v : null);
  return h(
    "div",
    {},
    h("div", { class: "row muted" }, h("span", {}, `max ${fmtValue(b.vMax / 1.1, unit)}`), h("span", {}, "latest")),
    svg,
    h("div", { class: "legend" }, shown.map((s, i) => h("span", { class: `c${i + 1}` }, `${s.label} ${fmtValue(latest(s), unit)}`))),
    series.length > shown.length ? h("div", { class: "small muted" }, `+${series.length - shown.length} more`) : null,
  );
}

const views = { overview: renderOverview, apps: renderApps, backups: renderBackups, load: renderLoad };

async function route() {
  const name = (location.hash || "#overview").slice(1);
  const view = views[name] || renderOverview;
  for (const a of document.querySelectorAll("#tabs a")) a.classList.toggle("active", a.getAttribute("href") === `#${name}`);
  try {
    await view();
  } catch (e) {
    fill(main, h("p", { class: "err" }, e.message));
  }
}

async function start() {
  try {
    config = await api("/api/config");
    document.getElementById("cluster").textContent = config.clusterName;
    document.title = `${config.clusterName} · clusterdeck`;
    document.getElementById("who").textContent = `${config.user}${config.actionsEnabled ? "" : " · read-only"}`;
  } catch (e) {
    fill(main, h("p", { class: "err" }, e.message));
    return;
  }
  window.addEventListener("hashchange", route);
  route();
}

start();
