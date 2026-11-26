<script>
  import { api, post } from "../lib/api.js";
  import { appHref, envOf, groupApps, matchesApp } from "../lib/format.js";
  import { act, show } from "../lib/notify.svelte.js";
  import Badge from "../components/Badge.svelte";

  let { config } = $props();
  let apps = $state(null);
  let error = $state("");
  const GROUPINGS = [["app", "App"], ["namespace", "Namespace"], ["none", "None"]];
  let groupBy = $state(load());
  let query = $state("");
  let shown = $derived(apps ? apps.filter((a) => matchesApp(a, query)) : []);
  let groups = $derived(groupApps(shown, groupBy));

  function load() {
    try {
      const v = localStorage.getItem("clusterdeck.apps.groupBy");
      if (GROUPINGS.some(([k]) => k === v)) return v;
    } catch {}
    return "app";
  }

  function setGroup(v) {
    groupBy = v;
    try { localStorage.setItem("clusterdeck.apps.groupBy", v); } catch {}
  }

  const unhealthy = (list) => list.filter((a) => !a.healthy).length;

  api("/api/apps").then((d) => (apps = d)).catch((e) => (error = e.message));

  async function logs(ns, pod, container) {
    try {
      show(`${ns}/${pod} · ${container}`, await api(`/api/pods/${ns}/${pod}/logs?tail=300&container=${encodeURIComponent(container)}`));
    } catch (e) {
      show("Logs failed", e.message);
    }
  }

  function restart(ns, w) {
    if (confirm(`Restart ${w.kind} ${ns}/${w.name}?`)) act("Restart", () => post(`/api/workloads/${ns}/${w.kind.toLowerCase()}/${w.name}/restart`));
  }
</script>

{#if error}<p class="err">{error}</p>
{:else if !apps}<p class="muted">Loading…</p>
{:else if !apps.length}<p class="muted">No workloads found.</p>
{:else}
  <h2 class="with-controls">Apps ({query.trim() ? `${shown.length} of ${apps.length}` : apps.length})
    <span class="segmented small">
      {#each GROUPINGS as [k, label]}
        <button class:on={groupBy === k} onclick={() => setGroup(k)}>{label}</button>
      {/each}
    </span>
  </h2>
  <div class="filter">
    <input type="search" placeholder="Filter apps…" title="Matches app, namespace, Argo CD app or workload names" aria-label="Filter apps" bind:value={query} />
  </div>
  {#if !shown.length}<p class="muted">No apps match “{query}”.</p>{/if}
  {#each groups as g (g.key)}
    <section class="app-group">
      {#if g.key}
        <h3 class="group-head">
          <span class="clamp2" title={g.key}>{g.key}</span>
          <span class="muted small">{g.apps.length} {g.apps.length === 1 ? "app" : "apps"}{#if groupBy === "app" && new Set(g.apps.map((a) => a.namespace)).size > 1} · {[...new Set(g.apps.map((a) => a.namespace))].join(", ")}{/if}</span>
          {#if unhealthy(g.apps)}<Badge status="failed" label="{unhealthy(g.apps)} unhealthy" />{/if}
        </h3>
      {/if}
      <div class="grid">
        {#each g.apps as a (a.namespace + "/" + a.name)}
          <div class="card">
            <h3><a class="plain clamp2" href={appHref(a.namespace, a.name)} title="{a.name} · {a.namespace}">{a.name} <span class="muted small">{a.namespace}</span></a><span class="tags">{#if envOf(a.namespace)}<span class="tag">{envOf(a.namespace)}</span>{/if}<Badge status={a.healthy ? "ok" : "failed"} /></span></h3>
            {#each a.argo as s}
              <div class="small argo">Argo CD <span class="mono clamp2" title={s.application}>{s.application}</span> <Badge status={s.sync} /> <Badge status={s.health} /></div>
            {/each}
            {#if a.restarts}<div class="small warn">{a.restarts} container restarts</div>{/if}
            {#each a.workloads as w (w.kind + w.name)}
              <div class="workload">
                <div class="row"><span class="mono clamp2" title="{w.kind.toLowerCase()}/{w.name}">{w.kind.toLowerCase()}/{w.name}</span><span class={w.ready < w.desired ? "bad" : "muted"}>{w.ready}/{w.desired}</span></div>
                <div class="small muted mono clamp2" title={w.images.join("\n")}>{w.images.join(", ")}</div>
                <ul class="pods">
                  {#each w.pods as p (p.name)}
                    <li class:multi={p.containers.length > 1}>
                      <span class="mono clamp2" class:warn={!p.ready} title={p.name}>{p.name}</span>
                      <span class="actions">
                        {#if p.restarts}<span class="warn">↻{p.restarts}</span>{/if}
                        {#each p.containers as c}
                          <button onclick={() => logs(a.namespace, p.name, c)}>{p.containers.length > 1 ? `Logs: ${c}` : "Logs"}</button>
                        {/each}
                      </span>
                    </li>
                  {/each}
                </ul>
                {#if config.actionsEnabled}
                  <div class="actions"><button onclick={() => restart(a.namespace, w)}>Rolling restart</button></div>
                {/if}
              </div>
            {/each}
            {#if a.workloads.some((w) => w.kind === "Deployment")}
              <div class="actions spaced"><a class="button" href={appHref(a.namespace, a.name)}>Rollouts →</a></div>
            {/if}
          </div>
        {/each}
      </div>
    </section>
  {/each}
{/if}
