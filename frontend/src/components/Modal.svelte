<script>
  let { open = $bindable(false), title, onclose, children } = $props();
  let el;
  $effect(() => {
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
  });
</script>

<dialog bind:this={el} onclose={() => { open = false; onclose?.(); }}>
  <h3>{title}</h3>
  {@render children?.()}
</dialog>
