<script>
  import { bounds, linePath, fmtValue } from "../lib/format.js";
  let { series, unit } = $props();
  const W = 300;
  const H = 150;
  const b = $derived(bounds(series));
  const shown = $derived(series.slice(0, 6));
  const latest = (s) => (s.points.length ? s.points[s.points.length - 1].v : null);
</script>

{#if !b}
  <div class="muted small">No data in this range.</div>
{:else}
  <div class="row muted"><span>max {fmtValue(b.vMax / 1.1, unit)}</span><span>latest</span></div>
  <svg viewBox="0 0 {W} {H}" preserveAspectRatio="none" role="img" aria-label="time series">
    {#each shown as s, i (s.label)}
      <path d={linePath(s.points, b, W, H)} fill="none" stroke="currentColor" stroke-width="1.5" vector-effect="non-scaling-stroke" class="c{i + 1}" />
    {/each}
  </svg>
  <div class="legend">
    {#each shown as s, i (s.label)}
      <span class="c{i + 1}">{s.label} {fmtValue(latest(s), unit)}</span>
    {/each}
  </div>
  {#if series.length > shown.length}<div class="small muted">+{series.length - shown.length} more</div>{/if}
{/if}
