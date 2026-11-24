<script>
  import { api } from "../lib/api.js";
  import { fmtBytes, fmtCores, fmtAgo } from "../lib/format.js";
  import Badge from "../components/Badge.svelte";
  import UsageBar from "../components/UsageBar.svelte";
  import DataTable from "../components/DataTable.svelte";

  let ov = $state(null);
  let error = $state("");
  api("/api/overview").then((d) => (ov = d)).catch((e) => (error = e.message));

  const ready = $derived(ov ? ov.nodes.filter((n) => n.ready).length : 0);
  const phases = $derived(ov?.podPhases ?? {});
  const nodeStatus = (n) => (n.ready ? (n.unschedulable ? "cordoned" : "Ready") : "NotReady");
</script>

{#if error}<p class="err">{error}</p>
{:else if !ov}<p class="muted">Loading…</p>
{:else}
  <h2>Cluster</h2>
  <div class="kpis">
    <div class="card kpi"><div class="v" class:bad={ready < ov.nodes.length}>{ready}/{ov.nodes.length}</div><div class="l">nodes ready</div></div>
    <div class="card kpi"><div class="v">{phases.Running || 0}</div><div class="l">pods running</div></div>
    <div class="card kpi"><div class="v" class:warn={phases.Pending}>{phases.Pending || 0}</div><div class="l">pods pending</div></div>
    <div class="card kpi"><div class="v" class:bad={phases.Failed}>{phases.Failed || 0}</div><div class="l">pods failed</div></div>
    <div class="card kpi"><div class="v">{ov.namespaces}</div><div class="l">namespaces</div></div>
  </div>

  <h2>Nodes</h2>
  {#if !ov.liveUsage}<p class="muted small">metrics-server is not answering, so live usage is not shown.</p>{/if}
  <div class="grid">
    {#each ov.nodes as n (n.name)}
      <div class="card">
        <h3>{n.name} <Badge status={nodeStatus(n)} /></h3>
        <div class="small muted">{n.roles.join(", ") || "worker"} · {n.version} · {n.podCount} pods</div>
        {#if n.pressure.length}<div class="small warn">{n.pressure.join(", ")}</div>{/if}
        <div class="row"><span>CPU</span><span class="muted">{fmtCores(n.cpuUsageMilli)} / {fmtCores(n.cpuAllocatableMilli)}</span></div>
        <UsageBar used={n.cpuUsageMilli} total={n.cpuAllocatableMilli} />
        <div class="row"><span>Memory</span><span class="muted">{fmtBytes(n.memUsageBytes)} / {fmtBytes(n.memAllocatableBytes)}</span></div>
        <UsageBar used={n.memUsageBytes} total={n.memAllocatableBytes} />
      </div>
    {/each}
  </div>

  <h2>Recent warnings</h2>
  {#if ov.warnings.length}
    <DataTable head={["When", "Object", "Reason", "Message"]}>
      {#each ov.warnings as e}
        <tr><td>{fmtAgo(e.time)}</td><td class="mono">{e.namespace}/{e.object}</td><td>{e.reason}</td><td>{e.message}</td></tr>
      {/each}
    </DataTable>
  {:else}<p class="muted">No warning events.</p>{/if}
{/if}
