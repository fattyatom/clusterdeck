<script>
  import { api, post } from "../lib/api.js";
  import { appHref } from "../lib/format.js";
  import { act, show } from "../lib/notify.svelte.js";
  import Badge from "../components/Badge.svelte";

  let { config } = $props();
  let apps = $state(null);
  let error = $state("");
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
  <h2>Apps ({apps.length})</h2>
  <div class="grid">
    {#each apps as a (a.namespace + "/" + a.name)}
      <div class="card">
        <h3><a class="plain" href={appHref(a.namespace, a.name)}>{a.name} <span class="muted small">{a.namespace}</span></a><Badge status={a.healthy ? "ok" : "failed"} /></h3>
        {#each a.argo as s}
          <div class="small">Argo CD <span class="mono">{s.application}</span> <Badge status={s.sync} /> <Badge status={s.health} /></div>
        {/each}
        {#if a.restarts}<div class="small warn">{a.restarts} container restarts</div>{/if}
        {#each a.workloads as w (w.kind + w.name)}
          <div class="workload">
            <div class="row"><span class="mono">{w.kind.toLowerCase()}/{w.name}</span><span class={w.ready < w.desired ? "bad" : "muted"}>{w.ready}/{w.desired}</span></div>
            <div class="small muted mono">{w.images.join(", ")}</div>
            <ul class="pods">
              {#each w.pods as p (p.name)}
                <li>
                  <span class="mono" class:warn={!p.ready}>{p.name}</span>
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
{/if}
