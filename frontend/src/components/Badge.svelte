<script>
  import { tone } from "../lib/format.js";
  let { status, label } = $props();
</script>

<span class="badge {tone(status)}">{label ?? status ?? "–"}</span>
