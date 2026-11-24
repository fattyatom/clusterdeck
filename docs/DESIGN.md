# Design

## The gap it fills

A GitOps cluster already has a deploy engine and its UI (Argo CD or Flux),
and usually a telemetry stack (SigNoz, Prometheus and Grafana). What it lacks
is the page Coolify and Dokploy give you for free: "is everything up, how
busy are the nodes, did last night's backups run, restore that one." On a
plain cluster those answers are spread over `kubectl get cronjobs`, the
telemetry UI and a runbook.

clusterdeck is that page and nothing else. Three rules keep it small:

1. **It never deploys.** Git and the GitOps controller own desired state.
   Anything clusterdeck changes is a new Job (backup, restore), the
   `restartedAt` annotation that `kubectl rollout restart` and Argo CD's own
   restart action write, or a rollback to a pod template the cluster already
   ran. It refuses a rollback the GitOps controller would undo.
2. **It never stores anything.** No database, no PVC. State comes from the
   Kubernetes API and an optional metrics backend on every request.
3. **Conventions are labels.** Backups are CronJobs, restores are Job
   templates in ConfigMaps. Any cluster can adopt them, and a cluster without
   them still gets the Overview, Apps and Load tabs.

## Where to run it

| | On the cluster | Off the cluster (a VM or a PaaS such as Dokploy) |
|---|---|---|
| Credentials | An in-cluster ServiceAccount; the token never leaves the cluster | A kubeconfig token for the cluster stored on another machine |
| Control-plane exposure | None beyond the ingress for the UI | The Kubernetes API must be reachable from that machine |
| When the cluster is down | Down too | Up, but can only say "API unreachable"; restores still need the cluster |
| Running backups and restores | Jobs run next to the data they mount | Same Jobs, created over a longer and more fragile path |
| Managed by | The same GitOps flow as every other app | A second deploy system |
| Blast radius if compromised | Bounded by its RBAC (read, plus Job create in chosen namespaces) | Same RBAC, plus whatever else that machine can reach |

**Recommendation: run it on the cluster**, read-only by default, actions
limited to the namespaces whose backups it manages, behind an auth proxy, and
LAN-only or behind an identity-aware tunnel.

The usual argument for off-cluster is "I need it when the cluster is broken".
That is the job of the telemetry and uptime stack, which should already run
outside the cluster and alert on its own. During a cluster outage the
dashboard cannot fix anything anyway: restores are Jobs, and Jobs need the
cluster. The break-glass path stays `kubectl`.

## Build or adopt

| Tool | Strong at | Why it is not this |
|---|---|---|
| Headlamp (the Kubernetes project's recommended UI since the official Dashboard was archived) | Generic resource browsing, plugins, OIDC | Resource-centric. It has no notion of backup health or restore templates; you would write a plugin for both. A good companion to clusterdeck. |
| Argo CD UI | Sync, health, resource tree, logs, restart | Per Application. It shows nothing about backups and has no cluster-wide load history. clusterdeck shows Argo CD status and leaves syncing to it. |
| Rancher | Multi-cluster management | Heavy (its own agents, CRDs, auth), and it wants to own the cluster. |
| Portainer | Docker and Kubernetes management | Deploy-centric; overlaps with GitOps. |
| Kubero, Devtron and other Kubernetes PaaS tools | Build and deploy from git | They are the deploy engine, which competes with Argo CD or Flux. |

## Security model

- Read access is cluster-wide: nodes, namespaces, pods, pod logs, events,
  ConfigMaps, workloads, Jobs, CronJobs, node metrics, Argo CD Applications.
  Never Secrets. Pod logs and ConfigMaps can still hold sensitive values, so
  treat access to the UI like access to `kubectl` with the `view` role.
- Write access exists only when `actions.enabled` is true: `create` on Jobs
  and Events, `patch` on Deployments, StatefulSets and DaemonSets, and
  `update` on Deployments (rollback), cluster-wide or only in
  `actions.namespaces`.
- Creating a Job in a namespace means running a pod with that namespace's
  service accounts and Secrets. That is why clusterdeck only runs a Job that
  someone has already declared: a CronJob carrying the backup label, or a
  ConfigMap carrying the restore-template label. Whoever can write those
  objects decides what clusterdeck may run, so keep them in git.
- Identity comes from a header set by an authenticating proxy. Enable the
  NetworkPolicy so nothing else can reach the pod and forge it.
- Actions need an identified user and a same-origin request header, and are
  logged as JSON (`"msg":"action"`) with the user, path and result.

## Rollback and GitOps

A rollback in the cluster and a rollback in git are different things. With
Argo CD self-heal on, only the second one sticks, so clusterdeck refuses the
first and shows which git revision to revert. A rollback that opens a revert
pull request would serve self-healed apps, at the cost of a git token. It is
not built.

## Not yet

- Scaling an app down before a restore and back up after it. Today a
  template's description has to tell the operator to do it.
- Listing backup files to pick from. Today `DUMP`-style parameters are typed
  in. Listing needs a read-only mount of the backup volume, or a small
  sidecar that serves the listing.
- Built-in OIDC for clusters without an auth proxy.
- Velero and CloudNativePG `Backup` resources as backup sources, beside
  CronJobs.
- Rollback for StatefulSets and DaemonSets (ControllerRevisions).
- Health from telemetry (error rate or latency in SigNoz) as an auto-rollback
  signal, beside pod state.
