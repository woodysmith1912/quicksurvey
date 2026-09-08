# Deploying QuickSurvey to DOKS

Manifests for the `DOKS` cluster. Eight objects in a new
`quicksurvey` namespace.

```sh
kubectl apply -k deploy/k8s
```

## Before you apply

**1. Point DNS at Traefik.** `survey.plainwrapworks.com` must resolve to
`<LB-IP>` — the `traefik-ingress-service` LoadBalancer. Traefik obtains
certificates by TLS-ALPN challenge, which resolves the hostname, so issuance
fails until DNS is live. Applying early is harmless; it just will not get a
certificate until you do.

**2. Publish the image.** The manifests reference
`ghcr.io/woodysmith1912/quicksurvey:v0.1.0`. Nothing in this cluster uses a
private registry or an `imagePullSecret`, so the package has to be public —
GHCR creates them private by default, and a private one fails with
`ImagePullBackOff` and an unhelpful message.

**3. Get the first password.** On first start with no accounts, the app creates
`admin` with a random password and prints it once:

```sh
kubectl -n quicksurvey logs statefulset/quicksurvey | grep password
```

It must be changed at first sign-in before the account can do anything.

## How this fits the cluster

Choices here follow what the cluster already does, not what a greenfield
deployment would do.

**Plain `Ingress`, no class, no `tls:` block.** Traefik runs with
`--providers.kubernetesIngress` and *not* `kubernetesCRD`, so an `IngressRoute`
would be accepted by the API server and silently never served. TLS is automatic:
Traefik is started with `--entrypoints.websecure.http.tls.certresolver=default`,
so every Ingress it picks up gets a Let's Encrypt certificate. Every existing
Ingress in this cluster looks exactly like this one. Adding a `tls:` section
would ask Traefik for a secret that nothing creates.

**`QS_REDIRECT_HTTPS=true`.** Traefik here serves `:80` without redirecting to
`:443` — there is no `entrypoints.web.http.redirections` in its arguments.
Cookies are marked `Secure`, and browsers do not send those over `http://`, so
a visitor arriving on the plain port could not sign in or vote and would get no
explanation. The app issues the redirect itself, with a 308 so a POSTed vote
keeps its method and body. `/healthz` is exempt, because kubelet probes the pod
directly over plain HTTP.

**StatefulSet, not Deployment.** The volume is `ReadWriteOnce`. A Deployment's
RollingUpdate starts the replacement before terminating the old pod, and if the
scheduler picks a different node the new pod hangs forever on `Multi-Attach
error`. A StatefulSet terminates first and waits. This bites on the first
rollout that happens to cross nodes — which is to say, not the first one, and
not reproducibly.

**`do-block-storage-retain`.** Already present in the cluster. Deleting the
StatefulSet or the PVC leaves the volume and every response on it. The default
class deletes.

**No PodDisruptionBudget.** With one replica, `minAvailable: 1` blocks node
drains indefinitely and turns every cluster upgrade into a manual step.

## Backups

A nightly CronJob creates a CSI `VolumeSnapshot` and keeps the newest 14.

Snapshots rather than a tar sidecar because the volume is `ReadWriteOnce`: a
second pod mounting it must land on the same node as the running one, and will
otherwise sit `Pending` forever. Snapshots happen inside the storage layer and
mount nothing.

A snapshot is crash-consistent rather than clean — SQLite in WAL mode keeps
recent commits in a side file — which is safe, because WAL recovery on next open
is exactly what WAL is for. For a guaranteed-clean file:

```sh
kubectl -n quicksurvey exec quicksurvey-0 -- \
  quicksurvey backup -to /data/backup-$(date +%F).db
kubectl -n quicksurvey cp quicksurvey-0:/data/backup-$(date +%F).db ./backup.db
```

The backup carries the instance HMAC key. Restoring without it invalidates every
session and voter cookie: responses survive, but returning respondents look new
and can vote again.

## Operating it

```sh
kubectl -n quicksurvey get pods,pvc,ingress
kubectl -n quicksurvey logs -f statefulset/quicksurvey
kubectl -n quicksurvey get volumesnapshot           # backups

# accounts, safe against the running server — SQLite serialises the writes
kubectl -n quicksurvey exec -it quicksurvey-0 -- quicksurvey user list
```

## If it does not start

| Symptom | Cause |
|---|---|
| `CrashLoopBackOff`, cannot create the database | `fsGroup` missing or ignored; the CSI volume arrives root-owned |
| `exec ...: operation not permitted` | `allowPrivilegeEscalation: false` colliding with an AppArmor profile transition. Seen on snap-packaged Docker; should not occur on DOKS, but that is the symptom |
| Pod `Pending`, `Multi-Attach error` | Two pods want the RWO volume. Should not happen with a StatefulSet; check nothing was scaled past one |
| `ImagePullBackOff` | The GHCR package is still private |
| Serves HTTP, no certificate | DNS not yet pointing at `<LB-IP>`, so the TLS-ALPN challenge cannot resolve |
