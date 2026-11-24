<script>
  let { open = $bindable(false), title, children } = $props();
  let el;
  $effect(() => {
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
  });
</script>

<dialog bind:this={el} onclose={() => (open = false)}>
  <h3>{title}</h3>
  {@render children?.()}
</dialog>
