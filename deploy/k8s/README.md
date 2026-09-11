# Deploying QuickSurvey to DOKS

Manifests for a DigitalOcean Kubernetes cluster. Eight objects in a new
`quicksurvey` namespace.

```sh
kubectl apply -k deploy/k8s
```

## Before you apply

**1. Point DNS at Traefik.** `quicksurveys.plainwrapworks.com` must resolve to
the external address of the `traefik-ingress-service` LoadBalancer:

```sh
kubectl get svc -A --field-selector metadata.name=traefik-ingress-service \
  -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}'
```

Create an A record for the host pointing at that address. The address is
stable for the life of the load balancer; recreating the LB changes it.

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
| a tag `v0.2.0` | `0.2.0`, `0.2`, `v0.2.0`, `latest`, `sha-<full-sha>` |

The manifests pin `0.2.0` — OCI tags conventionally drop the leading `v`, so
the git tag `v0.2.0` publishes the image `0.2.0` (and, since the tag-format fix,
`v0.2.0` as well). It exists once you have
pushed that git tag:

```sh
git tag v0.2.0 && git push origin v0.2.0
```

To deploy an untagged commit instead, point `newTag` in `kustomization.yaml` at
its `sha-<full-sha>`. Prefer either of those to `latest` or `main`: a moving tag
gives you no way to know what is running or to roll back. `latest` deliberately
moves only on a version tag, never on a push to `main`, so it cannot quietly
become an untested commit.

**The package must be public**, because nothing in this cluster uses a private
registry or an `imagePullSecret`. GHCR inherits the repository's visibility, so
a public repo gives a public package with nothing to do. Check it the way the
cluster will, without credentials:

```sh
R=woodysmith1912/quicksurvey
T=$(curl -s "https://ghcr.io/token?scope=repository:$R:pull&service=ghcr.io" | jq -r .token)
curl -s -H "Authorization: Bearer $T" "https://ghcr.io/v2/$R/tags/list"
```

A tag list means the cluster can pull it. A private package fails as
`ImagePullBackOff`, which does not say "it is private".

The image carries a build provenance attestation, so you can check where it came
from:

```sh
gh attestation verify oci://ghcr.io/woodysmith1912/quicksurvey:0.2.0 \
  --repo woodysmith1912/quicksurvey
```

**3. Get the first password.** On first start with no accounts, the app creates
`admin` with a random password and writes it to a file on the volume:

```sh
kubectl -n quicksurvey exec quicksurvey-0 -- quicksurvey initial-password
```

Not `cat`: the image is distroless and contains no `cat`, and no shell to run
one in. The binary is the only executable in it, so anything you need to do
inside the container has to be something the binary does.

Not the log. A log is shipped to aggregators, kept long after the password is
changed, and readable by anyone with `kubectl logs` on the namespace — a wider
audience than the volume. The file is mode 600 and is deleted the moment the
password is changed, which the account is forced to do at first sign-in.

## Changing the hostname

Two places, deliberately:

- `spec.rules[0].host` in `ingress.yaml`
- `QS_BASE_URL` in `statefulset.yaml`

CI fails if they disagree, so forgetting the second is caught before it ships.

An earlier version derived one from the other with a kustomize `replacements`
block. That was worse: it failed *broken*. If a future `kubectl` changed the
semantics of `replacements`, the placeholder would survive into production and
every editor would be handed a link to a host that does not exist. A literal is
already correct when the tooling misbehaves, and a check catches the mistake
that actually happens — a human editing one file and not the other.

Why `QS_BASE_URL` is set at all: without it the app reconstructs the origin from
`X-Forwarded-Host`. Setting it explicitly means the app never trusts a header a
client might try to influence.

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
  quicksurvey backup -to - > quicksurvey-$(date +%F).db
```

Streamed rather than copied: `kubectl cp` runs `tar` inside the container, and
there is no tar in a distroless image. `-to -` writes the snapshot to stdout,
which needs nothing in the container but the binary.

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
| Serves HTTP, no certificate | DNS not yet pointing at the load balancer, so the TLS-ALPN challenge cannot resolve |
