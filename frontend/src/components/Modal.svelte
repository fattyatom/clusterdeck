<script>
  let {
    open = $bindable(false),
    title,
    subtitle = "",
    size = "md",
    tone = "",
    onclose,
    children,
    toolbar,
    footer,
  } = $props();
  const id = $props.id();
  let el;
  let downOnBackdrop = false;

  $effect(() => {
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
  });
</script>

<dialog
  bind:this={el}
  class="modal {size} {tone}"
  aria-labelledby="{id}-title"
  onclose={() => { open = false; onclose?.(); }}
  onmousedown={(e) => (downOnBackdrop = e.target === el)}
  onclick={(e) => { if (downOnBackdrop && e.target === el) open = false; }}
>
  <header class="modal-head">
    <div class="modal-title">
      <h3 id="{id}-title">{#if tone}<span class="modal-dot" aria-hidden="true"></span>{/if}{title}</h3>
      {#if subtitle}<p class="muted small mono">{subtitle}</p>{/if}
    </div>
    <button class="icon" aria-label="Close" onclick={() => (open = false)}>✕</button>
  </header>
  {#if toolbar}<div class="modal-toolbar">{@render toolbar()}</div>{/if}
  <div class="modal-body">{@render children?.()}</div>
  {#if footer}<footer class="modal-foot">{@render footer()}</footer>{/if}
</dialog>
