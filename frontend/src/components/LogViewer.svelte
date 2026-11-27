<script>
  import { tick } from "svelte";
  import { api } from "../lib/api.js";
  import { LEVELS, RANGES, TAILS, logParams, rangeError, fmtLogTime, toggleLevel, highlight, summary, toLocalInput } from "../lib/logs.js";
  import Modal from "./Modal.svelte";

  let { open = $bindable(false), namespace, pod, containers = [], container = $bindable("") } = $props();

  let range = $state("1h");
  let from = $state(toLocalInput(Date.now() - 3600e3));
  let to = $state("");
  let levels = $state([]);
  let q = $state("");
  let tail = $state(500);
  let previous = $state(false);
  let wrap = $state(true);
  let live = $state(false);

  let data = $state(null);
  let error = $state("");
  let loading = $state(false);
  let scroller = $state();
  let seq = 0;

  const filters = $derived({ container, range, from, to, levels, q, tail, previous });
  const invalid = $derived(rangeError(filters));

  async function load() {
    if (!open || invalid) return;
    const mine = ++seq;
    const atBottom = !scroller || scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 40;
    loading = true;
    try {
      const out = await api(`/api/pods/${encodeURIComponent(namespace)}/${encodeURIComponent(pod)}/logs?${logParams(filters)}`);
      if (mine !== seq) return;
      data = out;
      error = "";
      if (atBottom) {
        await tick();
        scroller?.scrollTo({ top: scroller.scrollHeight });
      }
    } catch (e) {
      if (mine === seq) error = e.message;
    } finally {
      if (mine === seq) loading = false;
    }
  }

  $effect(() => {
    JSON.stringify(filters);
    if (!open) return;
    const timer = setTimeout(load, 250);
    return () => clearTimeout(timer);
  });

  $effect(() => {
    if (!open || !live || range === "custom") return;
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
  });

  $effect(() => {
    if (!open) {
      data = null;
      error = "";
      live = false;
    }
  });

  const count = (k) => data?.counts?.[k] ?? 0;
</script>

<Modal bind:open title="Logs" subtitle="{namespace}/{pod}{container ? ` · ${container}` : ''}" size="xl">
  {#snippet toolbar()}
    <div class="log-controls">
      {#if containers.length > 1}
        <label class="inline">Container
          <select bind:value={container}>
            {#each containers as c}<option value={c}>{c}</option>{/each}
          </select>
        </label>
      {/if}
      <label class="inline">Time
        <select bind:value={range}>
          {#each RANGES as [k, label]}<option value={k}>{k === "all" || k === "custom" ? label : `Last ${label}`}</option>{/each}
        </select>
      </label>
      {#if range === "custom"}
        <label class="inline">From <input type="datetime-local" bind:value={from} /></label>
        <label class="inline">To <input type="datetime-local" bind:value={to} /></label>
      {/if}
      <label class="inline grow">Search <input type="search" placeholder="Text the line contains" bind:value={q} /></label>
      <label class="inline">Lines
        <select bind:value={tail}>
          {#each TAILS as n}<option value={n}>{n}</option>{/each}
        </select>
      </label>
    </div>
    <div class="log-controls">
      <div class="chips" role="group" aria-label="Severity">
        {#each LEVELS as [k, label]}
          <button class="chip lvl-{k}" class:on={levels.includes(k)} aria-pressed={levels.includes(k)} onclick={() => (levels = toggleLevel(levels, k))}>
            {label}{#if data}<span class="chip-count">{count(k)}</span>{/if}
          </button>
        {/each}
        {#if levels.length}<button class="link" onclick={() => (levels = [])}>All severities</button>{/if}
      </div>
      <span class="spacer"></span>
      <label class="check"><input type="checkbox" bind:checked={previous} /> Previous container</label>
      <label class="check"><input type="checkbox" bind:checked={wrap} /> Wrap</label>
      <label class="check" title={range === "custom" ? "Live needs a relative time range" : "Refresh every 5 seconds"}><input type="checkbox" bind:checked={live} disabled={range === "custom"} /> Live</label>
      <button onclick={load} disabled={loading || !!invalid}>{loading ? "Loading…" : "Refresh"}</button>
    </div>
  {/snippet}

  {#if invalid}<p class="warn small">{invalid}</p>
  {:else if error}<p class="err">{error}</p>{/if}
  <div class="log-scroll" class:nowrap={!wrap} bind:this={scroller} aria-busy={loading}>
    {#if !data}
      <p class="muted small log-empty">{loading ? "Loading…" : ""}</p>
    {:else if !data.lines.length}
      <p class="muted small log-empty">No lines match these filters.</p>
    {:else}
      {#each data.lines as l, i (i)}
        <div class="log-line lvl-{l.level || 'unknown'}">
          <time datetime={l.time} title={l.time}>{fmtLogTime(l.time)}</time>
          <span class="log-lvl">{l.level ? l.level.toUpperCase() : ""}</span>
          <span class="log-msg">{#each highlight(l.text, q) as part}{#if part.hit}<mark>{part.text}</mark>{:else}{part.text}{/if}{/each}</span>
        </div>
      {/each}
    {/if}
  </div>

  {#snippet footer()}
    <span class="muted small">{summary(data, tail)}{live ? " · live" : ""}</span>
    <button onclick={() => (open = false)}>Close</button>
  {/snippet}
</Modal>
