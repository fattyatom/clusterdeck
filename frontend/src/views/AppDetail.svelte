<script>
  import { api, post } from "../lib/api.js";
  import { fmtAgo, shortRev, autoRollbackText } from "../lib/format.js";
  import { act } from "../lib/notify.svelte.js";
  import Badge from "../components/Badge.svelte";
  import DataTable from "../components/DataTable.svelte";

  let { config, namespace, app } = $props();
  let rollouts = $state(null);
  let error = $state("");
  let busy = $state(false);

  async function load() {
    try {
      rollouts = await api(`/api/rollouts/${encodeURIComponent(namespace)}/${encodeURIComponent(app)}`);
      error = "";
    } catch (e) {
      error = e.message;
    }
  }

  $effect(() => {
    load();
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
  });

  async function rollback(r, revision) {
    const target = revision ? `revision ${revision}` : "the previous revision";
    if (!confirm(`Roll back ${r.namespace}/${r.name} from revision ${r.revision} to ${target}?`)) return;
    busy = true;
    await act("Rollback", () => post(`/api/deployments/${r.namespace}/${r.name}/rollback`, { revision: revision || 0 }));
    busy = false;
    load();
  }

  const icon = { done: "✓", active: "●", failed: "✕", pending: "○" };
  const canAct = (r) => config.actionsEnabled && r.rollback.allowed && !busy;
</script>

<p class="crumbs"><a href="#apps">Apps</a> / <span class="mono">{namespace}</span> / <strong>{app}</strong></p>

{#if error}<p class="err">{error}</p>{/if}
{#if !rollouts}
  <p class="muted">Loading…</p>
{:else if !rollouts.length}
  <p class="muted">This app has no Deployments, so there is no rollout history to show.</p>
{:else}
  {#each rollouts as r (r.name)}
    <section class="card rollout">
      <h3>
        <span class="mono">deployment/{r.name}</span>
        <span class="actions"><span class="muted small">revision {r.revision}</span><Badge status={r.phase} /></span>
      </h3>

      {#if r.manager.kind === "argocd"}
        <div class="small muted">
          Argo CD <span class="mono">{r.manager.application}</span>
          · {r.manager.autoSync ? "auto-sync" : "manual sync"}{r.manager.selfHeal ? " · self-heal" : ""}
          · at <span class="mono">{shortRev(r.manager.syncRevision)}</span>
        </div>
      {:else}
        <div class="small muted">Not managed by Argo CD</div>
      {/if}

      <ol class="pipeline" aria-label="Rollout stages">
        {#each r.stages as s}
          <li class="stage {s.state}">
            <span class="stage-icon" aria-hidden="true">{icon[s.state]}</span>
            <span class="stage-name">{s.name}</span>
            <span class="stage-detail">{s.detail}</span>
          </li>
        {/each}
      </ol>
      {#if r.message}<p class="small muted">{r.message}</p>{/if}

      <div class="rollback-box">
        <div class="row">
          <strong>Rollback</strong>
          {#if config.actionsEnabled}
            <button class="danger" disabled={!canAct(r)} onclick={() => rollback(r, 0)}>Roll back to previous</button>
          {/if}
        </div>
        {#if !r.rollback.allowed}
          <p class="small warn">{r.rollback.reason}</p>
        {:else if r.rollback.reason}
          <p class="small muted">{r.rollback.reason}</p>
        {/if}
        {#if !config.actionsEnabled}<p class="small muted">Actions are off, so rollback is read-only here.</p>{/if}
        <p class="small muted">{autoRollbackText(r, config.autoRollback)}</p>
        {#if r.lastAutoRollback}
          <p class="small warn">Auto-rolled back {fmtAgo(r.lastAutoRollback.at)}: revision {r.lastAutoRollback.from} failed and the previous template was restored as revision {r.lastAutoRollback.to}.</p>
        {/if}
      </div>

      <h4>Revisions</h4>
      <DataTable head={["Rev", "Image", "Created", "Pods", ""]}>
        {#each r.history as h (h.revision)}
          <tr class:current={h.current}>
            <td class="rev">{h.revision}{#if h.current}<Badge status="ok" label="current" />{/if}</td>
            <td class="mono small">{h.images.join(", ")}{#if h.changeCause}<div class="muted">{h.changeCause}</div>{/if}</td>
            <td>{fmtAgo(h.created)}</td>
            <td>{h.ready}/{h.replicas}</td>
            <td>{#if config.actionsEnabled && !h.current}<button disabled={!canAct(r)} onclick={() => rollback(r, h.revision)}>Roll back here</button>{/if}</td>
          </tr>
        {/each}
      </DataTable>

      {#if r.manager.history.length}
        <h4>Argo CD syncs</h4>
        <DataTable head={["#", "Git revision", "Deployed"]}>
          {#each r.manager.history as s (s.id)}
            <tr><td>{s.id}</td><td class="mono">{shortRev(s.revision)}</td><td>{fmtAgo(s.deployedAt)}</td></tr>
          {/each}
        </DataTable>
      {/if}
    </section>
  {/each}
{/if}
