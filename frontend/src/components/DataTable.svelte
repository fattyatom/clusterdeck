<script>
  let { head, children } = $props();
</script>

<div class="table-wrap">
  <table>
    <thead><tr>{#each head as h}<th>{h}</th>{/each}</tr></thead>
    <tbody>{@render children()}</tbody>
  </table>
</div>
