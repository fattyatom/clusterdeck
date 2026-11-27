<script>
  import { api } from "./lib/api.js";
  import { parseRoute, linkify } from "./lib/format.js";
  import { notice, confirmDialog, settle } from "./lib/notify.svelte.js";
  import Modal from "./components/Modal.svelte";
  import Overview from "./views/Overview.svelte";
  import Apps from "./views/Apps.svelte";
  import AppDetail from "./views/AppDetail.svelte";
  import Backups from "./views/Backups.svelte";
  import Load from "./views/Load.svelte";

  let config = $state(null);
  let error = $state("");
  let route = $state(parseRoute(location.hash));

  api("/api/config")
    .then((c) => {
      config = c;
      document.title = `${c.clusterName} · clusterdeck`;
    })
    .catch((e) => (error = e.message));

  const tabs = [
    ["overview", "Overview"],
    ["apps", "Apps"],
    ["backups", "Backups"],
    ["load", "Load"],
  ];
  const activeTab = $derived(route.view === "app" ? "apps" : route.view);
</script>

<svelte:window
  onhashchange={() => {
    notice.open = false;
    settle(false);
    route = parseRoute(location.hash);
  }}
/>

<header class="top">
  <div class="brand"><span class="logo" aria-hidden="true"></span><span>{config?.clusterName ?? "clusterdeck"}</span></div>
  <nav aria-label="Sections">
    {#each tabs as [id, label]}
      <a href="#{id}" class:active={activeTab === id} aria-current={activeTab === id ? "page" : undefined}>{label}</a>
    {/each}
  </nav>
  {#if config}<div class="who">{config.user}{config.actionsEnabled ? "" : " · read-only"}{#if config.logout} · <a href={config.logout}>Sign out</a>{/if}</div>{/if}
</header>

<main>
  {#if error}
    <p class="err">{error}</p>
  {:else if config}
    {#if route.view === "overview"}<Overview />
    {:else if route.view === "apps"}<Apps {config} />
    {:else if route.view === "app"}{#key `${route.namespace}/${route.app}`}<AppDetail {config} namespace={route.namespace} app={route.app} />{/key}
    {:else if route.view === "backups"}<Backups {config} />
    {:else if route.view === "load"}<Load {config} />
    {/if}
  {/if}
</main>

<Modal bind:open={notice.open} title={notice.title} tone={notice.tone}>
  <pre class="output">{#each linkify(notice.text) as part}{#if part.href}<a href={part.href} target="_blank" rel="noopener noreferrer">{part.text}</a>{:else}{part.text}{/if}{/each}</pre>
  {#snippet footer()}<button class="primary" onclick={() => (notice.open = false)}>Close</button>{/snippet}
</Modal>

<Modal bind:open={confirmDialog.open} title={confirmDialog.title} size="sm" tone="bad" onclose={() => settle(false)}>
  <p class="confirm-text">{confirmDialog.text}</p>
  {#snippet footer()}<button onclick={() => settle(false)}>Cancel</button><button class="danger solid" onclick={() => settle(true)}>{confirmDialog.label}</button>{/snippet}
</Modal>
