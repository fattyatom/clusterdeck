<script>
  import { api } from "../lib/api.js";
  import { fmtAgo, tone, shortRev } from "../lib/format.js";
  import { fmtDuration, waitTone, waitText, change, stateTone, rowKey } from "../lib/inflight.js";
  import DataTable from "../components/DataTable.svelte";

  let report = $state(null);
  let error = $state("");

  function load() {
    api("/api/inflight")
      .then((r) => {
        report = r;
        error = "";
      })
      .catch((e) => (error = e.message));
  }
  load();
  $effect(() => {
    const t = setInterval(load, 60000);
    return () => clearInterval(t);
  });
</script>

{#if error}<p class="err">{error}</p>
{:else if !report}<p class="muted">Loading…</p>
{:else}
  <h2>In flight</h2>
  {#each report.notices as n}<div class="callout warn small">{n}</div>{/each}
  {#if report.rows.length}
    <DataTable head={["App", "Env", "Kind", "Change", "Health", "CI", "Soak", "Watch", "Argo CD", "Waiting on", "Since"]}>
      {#each report.rows as r (rowKey(r))}
        <tr>
          <td>
            <div>{r.app}</div>
            {#if r.pr}<div class="small"><a href={r.url} target="_blank" rel="noopener noreferrer">#{r.pr}</a>{#if r.title}{" "}<span class="muted">{r.title}</span>{/if}</div>{/if}
            {#each r.notes ?? [] as note}<div class="small warn">{note}</div>{/each}
          </td>
          <td>{r.env}</td>
          <td>{r.kind}</td>
          <td class="mono">{change(r)}</td>
          <td title={r.health_detail ?? ""}>{#if r.health}<span class="badge {stateTone(r.health)}">{r.health}</span>{:else}–{/if}</td>
          <td title={r.ci_detail ?? ""}>{#if r.ci}<span class="badge {stateTone(r.ci)}">{r.ci}</span>{:else}–{/if}</td>
          <td>{fmtDuration(r.soak_remaining_s)}</td>
          <td>{fmtDuration(r.watch_remaining_s)}</td>
          <td>
            {#if r.argo}
              <span class="badge {tone(r.argo.sync)}">{r.argo.sync || "–"}</span>
              <span class="badge {tone(r.argo.health)}">{r.argo.health || "–"}</span>
              <span class="mono muted">{shortRev(r.argo.revision)}</span>
            {:else}–{/if}
          </td>
          <td><span class="badge {waitTone(r)}">{waitText(r)}</span></td>
          <td>{r.since ? fmtAgo(r.since) : "–"}</td>
        </tr>
      {/each}
    </DataTable>
  {:else}
    <p class="muted">Nothing in flight: no open promotion or rollback pull requests, no watch windows, and every Argo CD application is synced and healthy.</p>
  {/if}
  <p class="small muted">Refreshed {fmtAgo(report.generated_at)}; the server caches for a minute. Soak {fmtDuration(report.soak_seconds)}, watch {fmtDuration(report.watch_seconds)}.</p>
{/if}
