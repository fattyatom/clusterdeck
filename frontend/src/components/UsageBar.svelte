<script>
  import { pct } from "../lib/format.js";
  let { used, total } = $props();
  const p = $derived(pct(used, total));
</script>

<div class="bar" role="meter" aria-valuenow={p == null ? undefined : Math.round(p)} aria-valuemin="0" aria-valuemax="100">
  <span class:hot={p > 75 && p <= 90} class:crit={p > 90} style:width="{p ?? 0}%"></span>
</div>
