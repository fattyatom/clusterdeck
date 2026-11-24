# clusterdeck

A small, self-hosted dashboard for a Kubernetes or k3s cluster. It covers the
parts a GitOps setup leaves without a page: **how loaded the cluster is, which
apps are unhealthy, whether the backups ran, and a guarded button to restore
one**.

clusterdeck is not a deploy tool. Argo CD, Flux or `kubectl apply` stay in
charge of what runs. clusterdeck reads the cluster and runs a few Jobs, so it
never fights your GitOps controller.

| Tab | What it shows | Where the data comes from |
|---|---|---|
| Overview | Node readiness, CPU and memory now, pod phases, recent warning events | Kubernetes API, metrics-server |
| Apps | Workloads grouped by `app.kubernetes.io/name`, ready replicas, restarts, pods, logs, Argo CD sync and health | Kubernetes API, Argo CD `Application`s (optional) |
| Backups | Every backup and restore-check CronJob: health, last success, next run, last 5 runs, **Run now** | CronJobs and Jobs you label |
| Restore | Restore templates with typed parameters and a typed confirmation | ConfigMaps you label |
| Load | CPU, memory and restarts by node and namespace over 1h to 7d | SigNoz or Prometheus (optional) |

Actions (Run now, Restore, Rolling restart) are **off by default**. When they
are on, every action is logged as JSON with the user who asked.

## Install

```bash
helm install clusterdeck deploy/helm/clusterdeck -n clusterdeck --create-namespace
kubectl -n clusterdeck port-forward svc/clusterdeck 8080:80
```

This is read-only and anonymous, which is fine behind a port-forward. Before
exposing it, put it behind an authenticating proxy (oauth2-proxy, Pomerium,
Authelia, Traefik forward-auth, ...) and tell clusterdeck which header carries
the user:

```yaml
auth:
  header: X-Forwarded-Email
  allowedUsers: [you@example.com]
networkPolicy:
  enabled: true
  ingressFromNamespaces: [ingress-nginx]
```

The NetworkPolicy matters: clusterdeck trusts the header, so only the proxy may
reach the pod. Then turn actions on, ideally only for the namespaces whose
backups you want to run:

```yaml
actions:
  enabled: true
  namespaces: [shop, photos]
```

## Labels (the whole contract)

| Object | Label | Optional annotations |
|---|---|---|
| Backup CronJob | `clusterdeck.io/role: backup` | `clusterdeck.io/description`, `clusterdeck.io/app` |
| Restore-check CronJob | `clusterdeck.io/role: restore-check` | same |
| Restore template ConfigMap | `clusterdeck.io/role: restore-template` | `clusterdeck.io/params` (comma-separated env names a user may set), `clusterdeck.io/description`, `clusterdeck.io/app` |

The selectors are configurable (`selectors.*` in the chart), so an existing
label such as `app.kubernetes.io/component=backup` works too.

A restore template is a ConfigMap whose `job.yaml` key holds a `batch/v1` Job.
**Restore** creates that Job in the template's own namespace, with the declared
parameters set as env vars on every container. clusterdeck refuses templates
that use host namespaces, hostPath volumes or privileged containers, refuses
undeclared parameters, and only accepts parameter values made of
`A-Z a-z 0-9 . _ : @ + = -` (no `..`). See `examples/`.

Keeping the Job inside a ConfigMap means your GitOps tool syncs the template,
never the destructive Job itself.

## Health of a backup

| Health | Meaning |
|---|---|
| ok | The last run succeeded and no scheduled run was missed |
| running | A run is in progress |
| failed | The most recent run failed |
| stale | Two scheduled runs have passed since the last success |
| never | It has existed for two scheduled runs and never succeeded |
| pending | New, has not had the chance to run yet |
| suspended | `spec.suspend: true` |

## Metrics

| `metrics.provider` | Needs | Metrics used |
|---|---|---|
| `none` | nothing | Overview still shows live usage from metrics-server |
| `signoz` | `url`, an API key Secret, optional `filter` | the OpenTelemetry `kubeletstats` and `k8s_cluster` receivers: `k8s.node.cpu.usage`, `k8s.node.memory.working_set`, `k8s.pod.cpu.usage`, `k8s.pod.memory.working_set`, `k8s.container.restarts` |
| `prometheus` | `url` | node-exporter, cAdvisor and kube-state-metrics series |

Use a SigNoz key with the Viewer role; clusterdeck only queries.

## Configuration

The binary reads environment variables; the chart sets them from values.

| Variable | Default |
|---|---|
| `LISTEN_ADDR` | `:8080` |
| `CLUSTER_NAME` | `cluster` |
| `KUBECONFIG` | in-cluster config |
| `AUTH_HEADER`, `AUTH_ALLOWED_USERS` | empty: anonymous, read-only |
| `ACTIONS_ENABLED` | `false`; needs `AUTH_HEADER` |
| `HIDE_NAMESPACES` | `kube-system,kube-public,kube-node-lease` (Apps tab only) |
| `BACKUP_SELECTOR`, `RESTORE_CHECK_SELECTOR`, `RESTORE_TEMPLATE_SELECTOR` | `clusterdeck.io/role=...` |
| `METRICS_PROVIDER` | `none` |
| `SIGNOZ_URL`, `SIGNOZ_API_KEY`, `SIGNOZ_FILTER` | |
| `PROMETHEUS_URL` | |

## Develop

```bash
go test ./...
node --test web/lib.test.mjs
go run ./hack/demo
```

`hack/demo` serves the real UI against a fake cluster on `:8099`, with actions
on. Run it from the repository root.

## Design and alternatives

See [docs/DESIGN.md](docs/DESIGN.md) for where to run it, what it deliberately
does not do, and how it compares with Headlamp, Rancher, Portainer and the
Argo CD UI.
