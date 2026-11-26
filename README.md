# clusterdeck

A small, self-hosted dashboard for a Kubernetes or k3s cluster. It covers the
parts a GitOps setup leaves without a page: **how loaded the cluster is, which
apps are unhealthy, how each rollout went, whether the backups ran, and
guarded buttons to roll back or restore**.

clusterdeck is not a deploy tool. Argo CD, Flux or `kubectl apply` stay in
charge of what runs. clusterdeck reads the cluster, runs a few Jobs, and rolls
a Deployment back in the cluster only where your GitOps controller will not
undo it. Where it would, the rollback is a pull request.

| Tab | What it shows | Where the data comes from |
|---|---|---|
| Overview | Node readiness, CPU and memory now, pod phases, recent warning events | Kubernetes API, metrics-server |
| Apps | Workloads grouped by `app.kubernetes.io/name`, ready replicas, restarts, pods, logs, Argo CD sync and health | Kubernetes API, Argo CD `Application`s (optional) |
| App → Rollouts | Per Deployment: a Synced → Rolled out → Available → Healthy pipeline, the last 10 revisions with their images, Argo CD sync history, **Roll back** to any revision, and opt-in **auto-rollback** | Deployments, ReplicaSets, pods, Argo CD `Application`s |
| Backups | Every backup and restore-check CronJob: health, last success, next run, last 5 runs, **Run now** | CronJobs and Jobs you label |
| Restore | Restore templates with typed parameters and a typed confirmation | ConfigMaps you label |
| Load | CPU, memory and restarts by node and namespace over 1h to 7d | SigNoz or Prometheus (optional) |

Actions (Run now, Restore, Rolling restart, Roll back) are **off by default**. When they
are on, every action is logged as JSON with the user who asked.

## Install

```bash
helm install clusterdeck deploy/helm/clusterdeck -n clusterdeck --create-namespace
kubectl -n clusterdeck port-forward svc/clusterdeck 8080:80
```

This is read-only and anonymous, which is fine behind a port-forward. Before
exposing it, give it an identity: either its own OpenID Connect login, or an
authenticating proxy in front of it.

### OpenID Connect login

Register clusterdeck as a confidential client with your identity provider
(Pocket ID, Authelia, Keycloak, Dex, ...), with the redirect URL
`https://<your host>/auth/callback`, and put the client secret in a Secret:

```yaml
auth:
  allowedUsers: [you@example.com]
  oidc:
    issuerURL: https://id.example.com
    clientID: clusterdeck
    redirectURL: https://deck.example.com/auth/callback
    existingSecret: clusterdeck-oidc
    clientSecretKey: client-secret
```

clusterdeck runs the authorization code flow with PKCE, checks the ID token's
signature, issuer, audience, expiry and nonce, and keeps the user in a signed,
`HttpOnly`, `SameSite=Lax` session cookie (12 h by default, `SESSION_TTL`).
The user is the `email` claim (`auth.oidc.userClaim`), and an email the
provider marks unverified is refused. Every page and API call then needs a
session; `/healthz` does not.

The cookie is signed with `SESSION_SECRET` (at least 32 characters, from
`auth.oidc.sessionSecretKey`). Without one, each start picks a random key, so
a restart signs everyone out; with single sign-on at the provider that only
costs a redirect.

### Authenticating proxy

Or put it behind oauth2-proxy, Pomerium, Authelia, Traefik forward-auth, ...
and tell clusterdeck which header carries the user:

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

## Rollouts and rollback

Each Deployment of an app gets a pipeline:

| Stage | Done when | Fails when |
|---|---|---|
| Synced (Argo CD) or Applied | Argo CD reports `Synced` | |
| Rolled out | Every replica runs the newest revision | The progress deadline passes, or a new pod is in `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `CreateContainerConfigError`, `CreateContainerError` or `InvalidImageName` |
| Available | Every replica is available | the rollout failed |
| Healthy | No new pod has a problem, and Argo CD (if any) reports `Healthy` | a new pod has one of the problems above |

**Roll back** does what `kubectl rollout undo` does: it puts an earlier
ReplicaSet's pod template back on the Deployment, and records a Kubernetes
Event saying who did it. Whether it is offered depends on who owns the
Deployment:

| Owner | Rollback |
|---|---|
| Nothing (plain `kubectl`, Helm) | Offered |
| Argo CD, manual sync | Offered |
| Argo CD, auto-sync without self-heal | Offered, with a warning: Argo CD shows the app `OutOfSync` and re-applies git on the next change |
| Argo CD, auto-sync **with self-heal** | **By pull request** when a GitHub token is configured (below); otherwise refused, because Argo CD would undo it within seconds |

clusterdeck finds the owning Argo CD Application from the
`argocd.argoproj.io/tracking-id` annotation, then the
`app.kubernetes.io/instance` label, then the Application deploying to the
Deployment's namespace.

### Rollback by pull request

With self-heal on, only git can roll an app back. When `gitops.github` is
configured, the rollback button on a self-healed app opens a pull request
instead of touching the cluster, and each earlier entry in the Argo CD sync
history gets a **Rollback PR to this** button.

The pull request restores the Application's `spec.source.path` to exactly what
it was at the chosen synced commit: changed files go back, files added since
are deleted, and nothing outside the path changes. It targets the
Application's `targetRevision` branch (the default branch for `HEAD`).
Nothing changes in the cluster until you merge it and Argo CD syncs.

Its description lists, each cut at 30 entries with "… and N more":

- **Files restored**: every file under the path that goes back or is deleted.
- **Not reverted**: files outside the path that changed since the chosen
  commit and that the Application can render from. clusterdeck reads the
  path's kustomization at the chosen commit and at the branch head and
  follows `resources`, `components`, `bases`, `patches`, generator `files`
  and the like, recursively and through relative paths such as `../base`. A
  referenced directory without a kustomization counts as a whole; remote
  references are skipped. When the path has no kustomization, or it cannot be
  read, the list falls back to changed files in the path's parent folder and
  its heading says so. GitHub reports at most 300 changed files per
  comparison; the description notes when that limit was hit.
- **Commits since**: commits on the branch after the chosen commit that
  touched the path or one of the files under *Not reverted* (the first 10
  of them), or, in the fallback, the parent folder. Other commits in a
  monorepo are left out.

| Supported | Not supported (the page says why) |
|---|---|
| One git source on github.com, with a `path`, tracking a branch | Several sources, Helm charts from a chart repository, other git hosts, a `targetRevision` that is a tag or commit |

clusterdeck authenticates in one of two ways. Either needs, on the
repositories your Applications use, **Contents: read and write** and **Pull
requests: read and write**. clusterdeck only creates branches named
`clusterdeck/rollback-*`; protect your target branch so a rollback still
needs a review to merge.

| Mode | Set | Pull requests and commits show | Notifications |
|---|---|---|---|
| **GitHub App** (preferred) | `GITHUB_APP_ID` + `GITHUB_APP_PRIVATE_KEY` | `<app-slug>[bot]` | native, like Renovate's |
| Token | `GITHUB_TOKEN` (a fine-grained personal access token) | the token's owner | none: GitHub never notifies you about your own activity |

With both set, the App wins. In App mode clusterdeck finds the App's
installation on each target repository, mints a one-hour installation token
scoped to that one repository, caches it, and re-mints it with less than 20
minutes left. Create the App with webhooks off, install it only on the
repositories you roll back, and give it the two permissions above.
`GITHUB_APP_PRIVATE_KEY` is the PEM, the PEM with `\n` escapes, or base64 of
the PEM.

```yaml
gitops:
  github:
    app:
      existingSecret: clusterdeck-github-app
      appIDKey: app-id
      privateKeyKey: private-key
    # or, with a personal access token:
    # existingSecret: clusterdeck-github
    # secretKey: token
```

The pod also needs outbound HTTPS to `api.github.com` (or your
`gitops.github.apiURL`).

RBAC cannot scope a pull request, so with `actions.namespaces` set,
clusterdeck itself refuses a rollback pull request for an Application whose
destination namespace is not in that list.

### Auto-rollback

**Auto-rollback** is opt-in twice: the install needs
`actions.autoRollback.enabled: true`, and each Deployment needs

```yaml
metadata:
  annotations:
    clusterdeck.io/auto-rollback: "true"
    clusterdeck.io/rollback-grace: 5m
```

Every 30 s, clusterdeck rolls a Deployment back to its previous revision when
the newest revision has failed (see the table above) for longer than the grace
period (default 5m), or at once when its progress deadline has passed. It never
rolls back a revision that is itself an auto-rollback, so a bad previous
revision cannot cause a loop: it stops and waits for a person. It records what
it did in `clusterdeck.io/auto-rollback-from`, `-to` and `-at` annotations, a
Warning Event and a log line.

For a self-healed app in pull-request mode, auto-rollback **opens** the
pull request to the previous synced commit and leaves merging to you. It opens
at most one per failing commit (the branch name carries that commit), and none
at all once a clusterdeck rollback of the app has landed after the previous
commit, so a rollback that still fails stops for a person rather than rolling
"back" to the bad commit. Only Deployments are covered; StatefulSets and
DaemonSets are not.

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
| `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_REDIRECT_URL` | empty: no login; all four or none, and not with `AUTH_HEADER` |
| `OIDC_USER_CLAIM`, `OIDC_SCOPES` | `email`, `openid,email,profile` |
| `SESSION_SECRET`, `SESSION_TTL` | random per start, `12h` |
| `ACTIONS_ENABLED` | `false`; needs `AUTH_HEADER` or OIDC login |
| `ACTIONS_NAMESPACES` | empty: all namespaces (auto-rollback scope and the Applications a rollback pull request may target, by destination namespace; the chart also scopes RBAC to it) |
| `AUTO_ROLLBACK_ENABLED`, `AUTO_ROLLBACK_INTERVAL` | `false`, `30s`; needs `ACTIONS_ENABLED` |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY` | empty; both or neither; the App wins over `GITHUB_TOKEN` |
| `GITHUB_TOKEN`, `GITHUB_API_URL` | empty (no rollback by pull request), `https://api.github.com` |
| `HIDE_NAMESPACES` | `kube-system,kube-public,kube-node-lease` (Apps tab only) |
| `BACKUP_SELECTOR`, `RESTORE_CHECK_SELECTOR`, `RESTORE_TEMPLATE_SELECTOR` | `clusterdeck.io/role=...` |
| `METRICS_PROVIDER` | `none` |
| `SIGNOZ_URL`, `SIGNOZ_API_KEY`, `SIGNOZ_FILTER` | |
| `PROMETHEUS_URL` | |

## Develop

The UI is Svelte 5, built by Vite into `web/dist`, which the Go binary embeds.
Build it before any Go command:

```bash
make test
make demo
```

`make demo` serves the real UI against a fake cluster on `:8099` with actions
on, including a failing rollout, an Argo CD self-healed app and an
auto-rollback record. For UI work, run it and `npm run dev` in `frontend/`
(Vite proxies `/api` to the demo).

## Design and alternatives

See [docs/DESIGN.md](docs/DESIGN.md) for where to run it, what it deliberately
does not do, and how it compares with Headlamp, Rancher, Portainer and the
Argo CD UI.
