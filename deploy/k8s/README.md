# Deploying QuickSurvey to DOKS

Manifests for the `DOKS` cluster. Eight objects in a new
`quicksurvey` namespace.

```sh
kubectl apply -k deploy/k8s
```

## Before you apply

**1. Point DNS at Traefik.** `quicksurveys.plainwrapworks.com` must resolve to
`<LB-IP>` — the `traefik-ingress-service` LoadBalancer. See
[../dns.md](../dns.md); the zone is in DigitalOcean and the records already
exist, but the delegation at the registrar still has to be pointed at
`ns1/ns2/ns3.digitalocean.com`.

Do this *before* applying. Traefik obtains certificates by TLS-ALPN challenge,
which resolves the hostname, and Let's Encrypt rate-limits failed validations at
5 per hostname per hour — applying against dead DNS spends those for nothing.

**2. Publish the image.** `.github/workflows/ci.yaml` builds and pushes to
`ghcr.io/woodysmith1912/quicksurvey`, but only after the Go tests, the browser
tests and the manifest render all pass.

Which tag it publishes depends on what you pushed:

| You push | CI publishes |
|---|---|
| a commit to `main` | `main`, `sha-<full-sha>` |
| a tag `v0.1.0` | `v0.1.0`, `0.1`, `latest`, `sha-<full-sha>` |

The manifests pin `v0.1.0`, which exists only once you have pushed that git
tag:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

To deploy an untagged commit instead, point `newTag` in `kustomization.yaml` at
its `sha-<full-sha>`. Prefer either of those to `latest` or `main`: a moving tag
gives you no way to know what is running or to roll back. `latest` deliberately
moves only on a version tag, never on a push to `main`, so it cannot quietly
become an untested commit.

**The package must be public.** Nothing in this cluster uses a private registry
or an `imagePullSecret`. GHCR creates packages private on first push — find it
under your GitHub profile → Packages → quicksurvey → Package settings → Change
visibility. A private one fails as `ImagePullBackOff` with an unhelpful message.

The image carries a build provenance attestation, so you can check where it came
from:

```sh
gh attestation verify oci://ghcr.io/woodysmith1912/quicksurvey:v0.1.0 \
  --repo woodysmith1912/quicksurvey
```

**3. Get the first password.** On first start with no accounts, the app creates
`admin` with a random password and prints it once:

```sh
kubectl -n quicksurvey logs statefulset/quicksurvey | grep password
```

It must be changed at first sign-in before the account can do anything.

## Changing the hostname

Edit `spec.rules[0].host` in `ingress.yaml`. Nothing else.

`QS_BASE_URL` in the StatefulSet is derived from it by a kustomize
`replacements` block, so the two cannot drift. That matters more than it looks:
if they disagree, the app still works, but every editor is handed a shareable
link pointing at a host that does not serve the survey — a failure nobody
notices until a respondent says the link is broken.

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
