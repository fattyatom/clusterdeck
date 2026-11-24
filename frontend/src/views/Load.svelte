<script>
  import { api } from "../lib/api.js";
  import Chart from "../components/Chart.svelte";

  let { config } = $props();
  let range = $state("6h");
  let results = $state({});

  $effect(() => {
    const r = range;
    results = {};
    for (const panel of config.panels) {
      api(`/api/metrics/${panel.id}?range=${r}`)
        .then((series) => r === range && (results[panel.id] = { series }))
        .catch((e) => r === range && (results[panel.id] = { error: e.message }));
    }
  });
</script>

{#if config.metricsProvider === "none"}
  <div class="notice">No metrics backend is configured. Set METRICS_PROVIDER to signoz or prometheus for history; the Overview tab still shows live usage from metrics-server.</div>
{:else}
  <h2 class="with-controls">
    Load · {config.metricsProvider}
    <span class="segmented" role="group" aria-label="Time range">
      {#each ["1h", "6h", "24h", "7d"] as r}
        <button class:on={r === range} aria-pressed={r === range} onclick={() => (range = r)}>{r}</button>
      {/each}
    </span>
  </h2>
  <div class="grid">
    {#each config.panels as panel (panel.id)}
      <div class="card chart">
        <h3>{panel.title}</h3>
        {#if !results[panel.id]}<div class="muted small">loading…</div>
        {:else if results[panel.id].error}<div class="err small">{results[panel.id].error}</div>
        {:else}<Chart series={results[panel.id].series} unit={panel.unit} />{/if}
      </div>
    {/each}
  </div>
{/if}
