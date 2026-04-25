## 5. First helm install

The chart lives in this repo at `helm/spire/`. The GKE-leaning overlay
ships at `helm/spire/values.gke.yaml` — it complements (does not
replace) `helm/spire/values.yaml`, so both render together when you
pass `-f helm/spire/values.gke.yaml`. Sections 1–4 of this runbook
already created the GKE cluster, the Artifact Registry images, the
DoltHub tower repo, the GCS bundle bucket, and the Workload Identity
binding from the `spire-operator` KSA to
`spire-tower@${PROJECT_ID}.iam.gserviceaccount.com`. This step turns
all of that into running pods.

### Canonical install command

This same invocation handles both the first install and every
subsequent upgrade — it is idempotent. Only the credential overrides
(below) and the image tag change between runs.

```bash
helm upgrade --install spire helm/spire \
  -n spire --create-namespace \
  -f helm/spire/values.gke.yaml
```

A bare invocation will fail the chart's own `required` gates: the GCS
bundle bucket must be set, the GCP credential placeholder needs to be
overridden, and DoltHub credentials must reach the Secret one way or
another. Add the override flags below before running.

### Required overrides

The bead description listed the override paths under a `secrets.*` /
`tower.*` shorthand for brevity; the real chart uses the keys below.
Pass these on the same `helm upgrade --install` line as the canonical
command above.

```bash
helm upgrade --install spire helm/spire \
  -n spire --create-namespace \
  -f helm/spire/values.gke.yaml \
  --set-string anthropic.apiKey="$ANTHROPIC_API_KEY" \
  --set-string github.token="$GITHUB_TOKEN" \
  --set-string dolthub.remoteUrl=acmeco/spire-tower \
  --set-string dolthub.user="$DOLTHUB_USER" \
  --set-string dolthub.password="$DOLTHUB_PASSWORD" \
  --set-string dolthub.credsKeyId="$DOLTHUB_KEY_ID" \
  --set-file   dolthub.credsKeyValue="$HOME/.dolt/creds/${DOLTHUB_KEY_ID}.jwk" \
  --set-string bundleStore.gcs.bucket=${PROJECT_ID}-spire-bundles \
  --set-file   gcp.serviceAccountJson=/dev/null \
  --set-string gateway.apiToken="$TOWER_TOKEN" \
  --set        gateway.ingress.enabled=true \
  --set-string gateway.ingress.host=spire.example.com
```

What each override does and where it lands:

- `anthropic.apiKey` — Anthropic classic API key. Rendered as
  `ANTHROPIC_API_KEY_DEFAULT` in the `spire-credentials` Secret. The
  steward sidecar's hand-rolled HTTP client reads this; wizard pods
  read it via `pkg/agent/pod_builder.go`'s AuthSlot routing. If you
  use a Claude subscription token instead, set
  `anthropic.subscriptionToken` (rendered as
  `ANTHROPIC_SUBSCRIPTION_TOKEN`).
- `github.token` — GitHub PAT used by wizard pods to clone, push, and
  open PRs. Rendered as `GITHUB_TOKEN`.
- `dolthub.remoteUrl` — the DoltHub repo that is the source of truth
  for this tower's beads (e.g. `acmeco/spire-tower`). The steward and
  the syncer both read this. Mandatory whenever `syncer.enabled=true`
  (the chart default).
- `dolthub.user` / `dolthub.password` — username and password for the
  cluster-side `remotesapi` SQL user that the post-install
  `spire-dolt-provision` Job creates. Laptops use these when they
  `dolt clone --user=<here>` against the in-cluster dolt server. The
  password must not contain a single quote (`'`); the provisioning
  Job rejects such passwords to avoid SQL/shell quoting pitfalls.
- `dolthub.credsKeyId` / `dolthub.credsKeyValue` — the Dolt key ID
  (`base32` string from `dolt creds ls`) and the raw JWK JSON from
  `~/.dolt/creds/<id>.jwk`. These authenticate HTTPS clone/pull/push
  to DoltHub itself (separate from the cluster-side remotesapi user
  above). Pass the JWK with `--set-file` so the JSON file's contents
  are embedded into the `<release>-dolthub-creds` Secret without
  shell-escaping pitfalls.
- `bundleStore.gcs.bucket` — the bucket created in section 4. The
  chart `required`s this whenever `bundleStore.backend=gcs` (which is
  the default in the GKE overlay). The Workload Identity binding from
  section 4 is what authorises the steward pod to read/write the
  bucket; the chart-level GCP credential below is a render-time
  placeholder, not a runtime credential.
- `gcp.serviceAccountJson` — `templates/steward.yaml` `required`s a
  non-empty value when `bundleStore.backend=gcs`, even on a
  pure-Workload-Identity install. Pass `/dev/null` (or any empty
  file) via `--set-file` to satisfy the gate; the runtime credential
  resolution still goes through Workload Identity because the
  `spire-operator` KSA is bound to the GSA. (TODO: verify whether the
  chart has since grown a `--no-gcp-key` opt-out — this gate is
  tracked for removal in the cluster-attach epic chain.)
- `gateway.apiToken` — Bearer token the gateway validates for every
  `/api/v1/*` request. The chart materialises this into the
  `spire-gateway-auth` Secret and the gateway container reads it via
  envFrom. If left empty, the gateway boots in dev mode (no auth) —
  fine behind a port-forward, **never** acceptable behind an Ingress.
  Generate with `openssl rand -base64 32` and store the value
  alongside whatever distributes credentials to operators.
- `gateway.ingress.enabled=true` and `gateway.ingress.host` — turn on
  the GKE Ingress (off by default in the overlay) and set the
  external hostname. The overlay already defaults
  `gateway.ingress.className=gce`, `managedCert.enabled=true`,
  `backendConfig.enabled=true`, and `backendConfig.http2=true`, so
  these two flags are usually all you need.

### Workload Identity is wired outside the chart

There is no `serviceAccount.googleServiceAccount` value on this
chart. The header of `helm/spire/values.gke.yaml` is the source of
truth: bind the `spire-operator` KSA in the `spire` namespace to
`spire-tower@${PROJECT_ID}.iam.gserviceaccount.com` with `gcloud iam
service-accounts add-iam-policy-binding` and `kubectl annotate
serviceaccount`, then the chart picks it up at runtime. Section 4
already covers the gcloud sequence; if you skipped it, go back —
otherwise the steward pod will land in `CrashLoopBackOff` with 403s
on every GCS call. (TODO: verify against the chart's eventual
"native WI" path — at that point this dance and the `gcp.json`
placeholder both become unnecessary.)

### Watch the rollout

The chart renders three workload deployments under
`-n spire`: `spire-steward` (singleton, on the PVC), `spire-gateway`
(scaled to `replicas: 2` in the GKE overlay, each on its own
emptyDir-backed `.beads/` workspace), and `spire-operator`. Watch
each:

```bash
kubectl -n spire rollout status deploy/spire-gateway --timeout=5m
kubectl -n spire rollout status deploy/spire-steward --timeout=5m
kubectl -n spire rollout status deploy/spire-operator --timeout=5m
```

Dolt comes up as a StatefulSet (`spire-dolt`); track it separately:

```bash
kubectl -n spire rollout status statefulset/spire-dolt --timeout=5m
```

If `spire-gateway` rolls out before `spire-dolt` is serving, its
`tower-attach` initContainer will block until the dolt SQL server
answers. That's expected — the chart sets `--dolt-wait=300s` so
first-install timing isn't a concern.

### Upgrades

Re-run the same `helm upgrade --install` invocation with the new
image tag (e.g. when a release of
`us-central1-docker.pkg.dev/${PROJECT_ID}/spire/spire:${TAG}` lands
in your Artifact Registry). The chart already pins workload tags to
`.Chart.AppVersion`, so updating the chart version is sufficient for
a coordinated bump; pass `--set images.steward.tag=${TAG}` and
`--set images.agent.tag=${TAG}` only when you need to override the
chart's pin to a hotfix tag. Section 11 covers the upgrade flow in
more detail.

## 6. Set up DNS and TLS

The GKE overlay turns on a **GKE Ingress + Google-managed
ManagedCertificate** (the chart's "Strategy A"). It does **not** use
Gateway API / `HTTPRoute` — verify that against the live templates
(`gateway-ingress.yaml`, `gateway-managedcert.yaml`) before debugging
TLS issues, because the resource names below assume the Ingress
strategy. (TODO: verify whether the chart has grown a
`GoogleClusterIssuer` / `CertificateMap` path; if so, the names in
this section need to be updated.)

### Get the Ingress external IP

The `gateway.ingress.host` value you passed at install (e.g.
`spire.example.com`) becomes the host on the Ingress. GCLB allocates
a public IP and writes it into `status.loadBalancer.ingress[0].ip`.
The Ingress is named `spire-gateway` (it shares its name with the
Service for backend wiring; see `gateway-ingress.yaml`).

```bash
kubectl -n spire get ingress spire-gateway \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
echo
```

Allow a few minutes after `helm install` for GCLB to provision the
load balancer and surface the IP. If the field is empty for more
than 5 minutes, `describe` the Ingress and look for events from the
`loadbalancer-controller`:

```bash
kubectl -n spire describe ingress spire-gateway
```

### Create the DNS records

Point `spire.example.com` at that IP at your DNS registrar. An A
record is required; an AAAA record is optional and only useful if
your registrar supports IPv6 LB IPs (GKE Ingress is IPv4-only by
default).

```bash
# At your DNS provider — example only, syntax varies.
# Type: A     Name: spire.example.com     Value: <ingress-ip>     TTL: 300
# Type: AAAA  Name: spire.example.com     Value: <ipv6-if-any>    TTL: 300
```

### Verify DNS propagation

```bash
dig +short spire.example.com
```

The output must match the Ingress IP you just looked up. If it
doesn't, wait for TTL to expire on whatever stale record you had,
then re-check. The managed certificate **will not** advance past
`Provisioning` until DNS resolves to the GCLB IP.

### Watch the managed certificate provision

The overlay defaults the ManagedCertificate name to
`spire-gateway-cert` (see `gateway.ingress.managedCert.name` in
`values.gke.yaml`). The Ingress wires it in via the
`networking.gke.io/managed-certificates` annotation.

```bash
kubectl -n spire get managedcertificate spire-gateway-cert -o yaml
kubectl -n spire describe managedcertificate spire-gateway-cert
```

Watch for `status.certificateStatus` to move
`Provisioning → Active`. Per-domain status under
`status.domainStatus[].status` should likewise reach `Active`.

Expected duration: **10–30 minutes** after DNS resolves. Google
re-checks DNS on its own cadence and only kicks the cert issuance
once it sees a matching A record from its prober. If the cert is
still `Provisioning` after 45 minutes:

- Re-confirm `dig +short spire.example.com` resolves to the Ingress IP.
- Check `status.domainStatus[].reason` for `FailedNotVisible` or
  `FailedCaaChecking`.
- Look in `kubectl describe managedcertificate spire-gateway-cert`
  Events for any `FailedToCreate` / `FailedToBind` records.

Section 12 (Troubleshooting) catalogues the more common failure
modes.

## 7. Verify

Once DNS resolves and the managed certificate is `Active`, walk the
checks below in order. Each one isolates a different layer (pods,
GCLB, gateway HTTP, gateway auth) so a failure points at exactly
one component.

### Pods

```bash
kubectl -n spire get pods
```

Every pod should be `Running` and have all containers `Ready` (e.g.
`2/2` for `spire-steward`, `1/1` for the rest). A pod stuck in
`Init:0/1` for more than a minute usually means the `tower-attach`
initContainer is still waiting on dolt — `kubectl logs` it to
confirm. A pod in `CrashLoopBackOff` after that is a real failure;
jump to section 12.

### Ingress / GCLB

```bash
kubectl -n spire get ingress spire-gateway
```

`ADDRESS` should be populated and match the IP your A record points
at. (The `kubectl get gateway` form referenced in some earlier docs
is for Gateway API resources; this chart ships an Ingress, so
`get ingress` is the right verb.)

You can also verify GCLB sees the backend as healthy:

```bash
kubectl -n spire describe ingress spire-gateway | grep -A2 'Backends:'
```

A backend reported `UNHEALTHY` here usually means the BackendConfig
health check (`/healthz` on port `3030`, the `gateway.apiPort`) is
failing — confirm the gateway pods themselves answer it (next
check).

### Gateway HTTP — public, unauthenticated

```bash
curl -fsS https://spire.example.com/healthz
```

Expect `HTTP/2 200` and a small JSON body with the gateway version.
This proves DNS, TLS termination at the GCLB, the GKE Ingress
routing, and the gateway pod's `/healthz` handler — in one shot.
TLS errors here mean the managed cert is not yet `Active`; 502s
mean GCLB cannot reach the backend (BackendConfig / health check
mismatch); 404s mean the Ingress routes did not render (re-check
`gateway.ingress.enabled=true`).

### Gateway HTTP — authenticated

The `/api/v1/tower` route returns tower metadata to authenticated
callers. Use the same `$TOWER_TOKEN` you passed to
`gateway.apiToken` at install:

```bash
curl -fsS -H "Authorization: Bearer $TOWER_TOKEN" \
  https://spire.example.com/api/v1/tower
```

Expect a JSON document containing the tower's `id`, `prefix`,
`database`, and `dolthubRemote`. A `401` means the token does not
match what the gateway validates (re-set `gateway.apiToken` and
re-run `helm upgrade`); a `403` means the gateway's auth path
matched but rejected the request body — usually a mismatched audience
on a JWT, which the v1 token model does not use, so re-check that
you are passing the bare bearer string and not a JWT.

### Retrieving the tower token

If you forgot or never persisted the value you passed to
`gateway.apiToken`, retrieve it from the chart-rendered Secret:

```bash
kubectl -n spire get secret spire-gateway-auth \
  -o jsonpath='{.data.SPIRE_API_TOKEN}' | base64 -d
echo
```

The Secret name is `spire-gateway-auth` (chart default; defined by
the `spire.gatewaySecretName` helper) and the key inside it is
`SPIRE_API_TOKEN`. (TODO: verify that the chart still surfaces this
via `helm get notes spire -n spire` — the NOTES.txt template is
slated to print the retrieval command in a later epic.) Note that
the chart does **not** auto-generate a token: if you installed
without `--set-string gateway.apiToken=...`, the Secret value is
empty and the gateway is running in dev mode (no auth). Re-run
`helm upgrade` with a real token before exposing the Ingress to
anything beyond a port-forward.

With all four checks green, the tower is up and ready for the CLI
attach in section 8.
