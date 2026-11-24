<script>
  import { api, post } from "../lib/api.js";
  import { fmtAgo, tone } from "../lib/format.js";
  import { act } from "../lib/notify.svelte.js";
  import Badge from "../components/Badge.svelte";
  import DataTable from "../components/DataTable.svelte";
  import Modal from "../components/Modal.svelte";

  let { config } = $props();
  let jobs = $state(null);
  let templates = $state([]);
  let error = $state("");

  let restoreOpen = $state(false);
  let current = $state(null);
  let values = $state({});
  let confirmText = $state("");

  function load() {
    Promise.all([api("/api/backups"), api("/api/restores")])
      .then(([j, t]) => {
        jobs = j;
        templates = t;
      })
      .catch((e) => (error = e.message));
  }
  load();

  function runNow(j) {
    if (confirm(`Run ${j.namespace}/${j.name} now?`)) act("Run now", () => post(`/api/cronjobs/${j.namespace}/${j.name}/run`)).then(load);
  }

  function openRestore(t) {
    current = t;
    values = Object.fromEntries(t.params.map((p) => [p.name, p.default]));
    confirmText = "";
    restoreOpen = true;
  }

  const target = $derived(current ? `${current.namespace}/${current.name}` : "");

  function runRestore() {
    const params = Object.fromEntries(Object.entries(values).filter(([, v]) => v !== ""));
    restoreOpen = false;
    act("Restore", () => post(`/api/restores/${current.namespace}/${current.name}/run`, { confirm: confirmText, params }));
  }
</script>

{#if error}<p class="err">{error}</p>
{:else if !jobs}<p class="muted">Loading…</p>
{:else}
  <h2>Backups & restore checks</h2>
  {#if jobs.length}
    <DataTable head={["Job", "Kind", "Schedule", "Health", "Last success", "Next run", "Recent", ""]}>
      {#each jobs as j (j.namespace + "/" + j.name)}
        <tr>
          <td><div class="mono">{j.namespace}/{j.name}</div>{#if j.description}<div class="small muted">{j.description}</div>{/if}</td>
          <td>{j.role === "backup" ? "backup" : "restore check"}</td>
          <td class="mono">{j.schedule}{j.timeZone ? ` (${j.timeZone})` : ""}</td>
          <td><Badge status={j.health} /></td>
          <td>{fmtAgo(j.lastSuccess)}</td>
          <td>{j.nextRun ? fmtAgo(j.nextRun) : "–"}</td>
          <td><span class="dots" title="newest first">{#each j.recent as r (r.name)}<span class="dot {tone(r.state)}" title="{r.name}: {r.state}{r.manual ? ' (manual)' : ''}"></span>{/each}</span></td>
          <td>{#if config.actionsEnabled}<button onclick={() => runNow(j)}>Run now</button>{/if}</td>
        </tr>
      {/each}
    </DataTable>
  {:else}
    <p class="muted">No CronJobs carry the backup or restore-check label. See the README for the labels.</p>
  {/if}

  <h2>Restore</h2>
  {#if templates.length}
    <DataTable head={["Template", "App", "Parameters", ""]}>
      {#each templates as t (t.namespace + "/" + t.name)}
        <tr>
          <td>
            <div class="mono">{t.namespace}/{t.name}</div>
            {#if t.description}<div class="small muted">{t.description}</div>{/if}
            {#if t.error}<div class="small err">{t.error}</div>{/if}
          </td>
          <td>{t.app || "–"}</td>
          <td>{t.params.map((p) => p.name).join(", ") || "–"}</td>
          <td>{#if config.actionsEnabled && !t.error}<button class="danger" onclick={() => openRestore(t)}>Restore…</button>{/if}</td>
        </tr>
      {/each}
    </DataTable>
  {:else}
    <p class="muted">No restore templates found.</p>
  {/if}
{/if}

<Modal bind:open={restoreOpen} title="Restore {target}">
  {#if current}
    <p class="warn">A restore usually overwrites live data. Read the template's description before running it.</p>
    {#if current.description}<p class="muted">{current.description}</p>{/if}
    {#each current.params as p (p.name)}
      <label for="param-{p.name}">{p.name}</label>
      <input id="param-{p.name}" name={p.name} autocomplete="off" placeholder={p.default || "(template default)"} bind:value={values[p.name]} />
    {/each}
    <label for="restore-confirm">Type {target} to confirm</label>
    <input id="restore-confirm" autocomplete="off" placeholder={target} bind:value={confirmText} />
    <div class="actions spaced">
      <button class="danger" disabled={confirmText !== target} onclick={runRestore}>Run restore</button>
      <button onclick={() => (restoreOpen = false)}>Cancel</button>
    </div>
  {/if}
</Modal>
