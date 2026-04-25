## 12. Troubleshooting

Each subsection below maps a real failure mode hit during E1-E5 development to a
diagnose-and-fix recipe. Symptoms are what an operator actually sees; diagnose
commands narrow the cause; fixes are copy-pasteable.

If you hit something not covered here, dump the offending pod's events and
logs first — `kubectl describe pod <name> -n spire` and `kubectl logs <name>
-n spire --all-containers --previous` — then file an issue against the runbook
with the symptom you saw and the cause you eventually found.

### Wizard / steward / gateway pod can't read or write GCS (Workload Identity not bound)

**Symptom:** the affected pod logs show one of:

```
googleapi: Error 401: Anonymous caller does not have storage.objects.create access ...
googleapi: Error 403: Caller does not have storage.objects.list access to bucket ...
google: could not find default credentials. See https://cloud.google.com/docs/authentication ...
```

`kubectl logs deploy/spire-steward -n spire -c steward` (or the wizard pod's
`agent` container) is the place these surface most often.

**Likely cause:** the Kubernetes ServiceAccount used by the pod is not bound
to the Google Service Account that holds `roles/storage.objectAdmin` on the
bundle bucket. Either the `iam.gke.io/gcp-service-account` annotation is
missing on the KSA, or the GSA's IAM policy never got the
`roles/iam.workloadIdentityUser` member that lets the KSA impersonate it.

**Diagnose:**

```bash
# Confirm the KSA carries the WI annotation
kubectl get sa spire -n spire -o jsonpath='{.metadata.annotations.iam\.gke\.io/gcp-service-account}'
# Expect: spire-bundles@<project>.iam.gserviceaccount.com

# Confirm the GSA's IAM policy lists the KSA as a workloadIdentityUser
gcloud iam service-accounts get-iam-policy \
  spire-bundles@<project>.iam.gserviceaccount.com \
  --project=<project> \
  --format='table(bindings.role,bindings.members)'
# Expect a binding "roles/iam.workloadIdentityUser" with member
# "serviceAccount:<project>.svc.id.goog[spire/spire]"

# From inside the pod, prove the metadata server is returning a token for
# the expected GSA.
kubectl exec deploy/spire-steward -n spire -c steward -- \
  curl -sS -H "Metadata-Flavor: Google" \
  http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/email
# Expect: spire-bundles@<project>.iam.gserviceaccount.com
```

If the email comes back as the node default service account
(`<projectnumber>-compute@developer.gserviceaccount.com`), Workload Identity
isn't taking effect for this pod — the KSA annotation, the node-pool
`GKE_METADATA` setting, or the cluster-level `--workload-pool` is wrong.

**Fix:**

```bash
# Bind the KSA → GSA (one-time per namespace/SA)
gcloud iam service-accounts add-iam-policy-binding \
  spire-bundles@<project>.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser \
  --member="serviceAccount:<project>.svc.id.goog[spire/spire]" \
  --project=<project>

# Annotate the KSA to declare which GSA to impersonate
kubectl annotate serviceaccount spire -n spire \
  iam.gke.io/gcp-service-account=spire-bundles@<project>.iam.gserviceaccount.com \
  --overwrite

# Rotate pods so they pick up the fresh token
kubectl rollout restart deploy/spire-steward deploy/spire-gateway -n spire
```

If the cluster itself isn't WI-enabled, recreate it with
`--workload-pool=<project>.svc.id.goog` (cluster-level) and the node pool
with `--workload-metadata=GKE_METADATA`.

### ManagedCertificate stuck in `Provisioning`

**Symptom:** `kubectl describe managedcertificate spire-gateway-cert -n spire`
keeps showing `Status: Provisioning` (sometimes for hours) and `curl
https://spire.<domain>/healthz` returns a TLS error or connection reset.

**Likely cause:** Google's managed-cert controller cannot complete the
HTTP-01 challenge because the gateway's external IP is not yet live in DNS,
or DNS still points somewhere else. The cert sits in `Provisioning` until DNS
resolves to the Ingress/Gateway IP.

**Diagnose:**

```bash
# Inspect the cert resource for the live status reason
kubectl describe managedcertificate spire-gateway-cert -n spire

# Resolve DNS from outside the cluster
dig +short spire.<domain>

# Get the actual Ingress / Gateway external IP
kubectl get ingress -n spire
# or, on Gateway API:
kubectl get gateway -n spire -o jsonpath='{.items[0].status.addresses[*].value}'

# Reserved static IP (if you used one)
gcloud compute addresses describe spire-gateway --global \
  --format='value(address)' --project=<project>
```

The IP returned by `dig` MUST equal the IP on the Ingress/Gateway. If they
differ, DNS hasn't propagated or the A record points at the wrong target.

**Fix:**

```bash
# Update the A record at your DNS provider to the gateway external IP, then
# wait for propagation (usually <5min, occasionally up to TTL).

# Once dig matches the gateway IP, the cert typically goes Active within
# 15-60 minutes. To prod the controller along, recreate the cert:
kubectl delete managedcertificate spire-gateway-cert -n spire
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml
```

If the cert refuses to leave `Provisioning` after an hour with DNS pointing
correctly, check `kubectl describe managedcertificate ...` for events
mentioning `FailedNotVisible` — that's Google's way of saying the
HTTP-01 challenge can't reach `/.well-known/acme-challenge/...`. Confirm the
Ingress accepts HTTP on port 80 (managed certs require port 80 reachable for
the challenge).

### Gateway has no external IP / Ingress addr is empty

**Symptom:** `kubectl get gateway -n spire` shows no `ADDRESS`, or
`kubectl get ingress -n spire` shows `<none>` under `ADDRESS` indefinitely.
Pods are healthy but unreachable from outside the cluster.

**Likely cause:** the Gateway API addon is disabled on the cluster, the
Gateway API CRDs (`gateway.networking.k8s.io`) aren't installed, or the
GKE Gateway controller can't reconcile the resource (often missing
`gke-l7-global-external-managed` GatewayClass).

**Diagnose:**

```bash
# Confirm the Gateway API CRDs are installed
kubectl get crds | grep gateway.networking.k8s.io
# Expect: gateways, gatewayclasses, httproutes at minimum

# Confirm the GKE GatewayClasses exist
kubectl get gatewayclasses
# Expect at least gke-l7-global-external-managed (or gke-l7-rilb for internal)

# Confirm the GKE gateway controller pods are running
kubectl get pods -n gke-system -l k8s-app=gke-l7-gateway-controller
# (namespace and label may differ on older clusters; on newer it's
# `kube-system` with `app=gke-gateway-controller`)

# Look at events on the Gateway resource itself
kubectl describe gateway spire -n spire
```

If the GatewayClass is missing or the controller pods aren't running, the
addon isn't installed.

**Fix:**

```bash
# Enable the Gateway API addon on the cluster (one-time)
gcloud container clusters update <cluster-name> \
  --gateway-api=standard \
  --location=<zone-or-region> \
  --project=<project>

# Re-apply your Helm release; the Gateway resource should pick up an IP
# within 1-2 minutes of the controller becoming ready.
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml
kubectl get gateway -n spire -w
```

If you'd rather use classic Ingress, set
`gateway.ingress.className=gce` in your overlay and the chart provisions a
GCE Ingress instead of a Gateway resource — covered under [Set up DNS and TLS](#6-set-up-dns-and-tls).

### Image pull errors (`ErrImagePull` / `ImagePullBackOff`)

**Symptom:** `kubectl get pods -n spire` shows pods stuck in
`ErrImagePull` or `ImagePullBackOff`. `kubectl describe pod <name> -n spire`
shows events like:

```
Failed to pull image "us-central1-docker.pkg.dev/<project>/spire/agent:latest":
  rpc error: code = NotFound desc = ... was not found
Failed to pull image "us-central1-docker.pkg.dev/<project>/spire/agent:latest":
  rpc error: code = Unauthenticated desc = ... unauthorized to access this resource
```

**Likely cause:**

1. The Artifact Registry repository wasn't created (`NotFound`), or
2. The image was pushed to a different region than the cluster pulls from
   (`NotFound` on a region mismatch), or
3. The cluster's nodes / pod ServiceAccount doesn't have
   `roles/artifactregistry.reader` on the GAR repo (`Unauthenticated`).

**Diagnose:**

```bash
# Confirm the repo exists in the expected region
gcloud artifacts repositories list --project=<project> \
  --format='table(name,format,location)'

# Confirm the image+tag is actually present
gcloud artifacts docker images list \
  <region>-docker.pkg.dev/<project>/spire \
  --include-tags

# Inspect the pod's actual image reference
kubectl get pod <name> -n spire -o jsonpath='{.spec.containers[*].image}'
# Compare region/project against the gcloud listing

# If pull auth is the issue, dump the pod events
kubectl describe pod <name> -n spire | grep -A5 -i "pull\|unauth"
```

**Fix:**

```bash
# (a) Repo missing — create it once per project (see runbook section 3)
gcloud artifacts repositories create spire \
  --repository-format=docker \
  --location=<region> \
  --project=<project>

# (b) Region mismatch — re-push in the cluster's region or update
# images.{steward,agent}.repository in your overlay to point at where the
# images actually live, then helm upgrade.

# (c) Auth missing on a same-project GKE cluster — grant the node SA reader
gcloud artifacts repositories add-iam-policy-binding spire \
  --location=<region> \
  --member="serviceAccount:<node-default-sa>" \
  --role=roles/artifactregistry.reader \
  --project=<project>
kubectl rollout restart deploy/spire-steward deploy/spire-operator -n spire
```

For cross-project clusters or non-GKE, see
[runbooks/gar-image-registry.md](runbooks/gar-image-registry.md) sections 3B
and 3C.

### `helm install` times out waiting for resources

**Symptom:** `helm install spire ...` hangs and eventually fails with:

```
Error: INSTALLATION FAILED: timed out waiting for the condition
```

`kubectl get pods -n spire` shows one or more pods stuck in `Pending` or
`ContainerCreating`.

**Likely cause:** the cluster has insufficient capacity to schedule the
requested pods (CPU/memory requests exceed available node allocatable),
or a PVC can't bind because no StorageClass / no volumes available.

**Diagnose:**

```bash
# Check pending pods cluster-wide
kubectl get pods -A | grep -i pending

# See WHY a specific pod is pending — events at the bottom
kubectl describe pod <pending-pod> -n spire | tail -30
# Common phrases: "0/N nodes are available: insufficient cpu",
#                 "insufficient memory", "had untolerated taint",
#                 "pod has unbound immediate PersistentVolumeClaims"

# Inspect node capacity vs allocations
kubectl top nodes
kubectl describe nodes | grep -A4 "Allocated resources"

# PVC status
kubectl get pvc -n spire
# If any are Pending, describe to see why
kubectl describe pvc <pvc-name> -n spire
```

**Fix:**

```bash
# (a) Add a node pool or scale up the existing one
gcloud container node-pools create extra \
  --cluster=<cluster-name> \
  --machine-type=e2-standard-4 \
  --num-nodes=2 \
  --location=<zone-or-region> \
  --project=<project>

# (b) Lower request floors via overlay if the defaults are over-eager,
# then helm upgrade. Don't lower below documented minimums for steward
# and dolt — they need real RAM headroom to run.

# (c) For unbound PVCs, set a default StorageClass on the cluster or
# specify storageClass: standard-rwo in the overlay's dolt.storage block.
kubectl patch storageclass standard-rwo \
  -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
```

### Dolt push fails with auth error / 403

**Symptom:** the dolt sync sidecar (or the dolt-backup CronJob) logs:

```
remote.SyncError: failed to push to remote 'origin':
  fatal: authentication failed for https://doltremoteapi.dolthub.com/<org>/<repo>
```

Or `spire push` from outside the cluster returns:

```
Error: failed to push: 403 Forbidden
```

**Likely cause:** the `doltCreds` Secret holds a key that's no longer valid
on DoltHub, the `dolthub.userName` configured on the cluster doesn't match
the account that owns the JWK, or the DoltHub repo's Collaborators list
doesn't include the account.

**Diagnose:**

```bash
# Confirm the Secret keys are present
kubectl get secret spire-dolthub-creds -n spire -o jsonpath='{.data}' | jq 'keys'
# Expect: ["creds-key-id", "creds-key-value", "user-email", "user-name"]

# Read the configured username (decode base64)
kubectl get secret spire-dolthub-creds -n spire \
  -o jsonpath='{.data.user-name}' | base64 -d

# Read the key ID and confirm it matches what `dolt creds ls` shows on the
# laptop the JWK was minted on
kubectl get secret spire-dolthub-creds -n spire \
  -o jsonpath='{.data.creds-key-id}' | base64 -d

# Test push from inside the dolt pod (cleanest way to isolate auth)
kubectl exec -n spire deploy/spire-dolt -- \
  dolt push origin main 2>&1 | head -20
```

Visit DoltHub → repo settings → Collaborators and confirm the configured
account is listed with write access.

**Fix:**

```bash
# Rotate the JWK on the laptop that owns the DoltHub account
dolt creds new
dolt creds use <new-key-id>
# (Push the new public key to DoltHub via the web UI, then test from laptop)

# Update the cluster Secret to the new key ID + JWK
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set dolthub.credsKeyId=<new-key-id> \
  --set-file dolthub.credsKeyValue=$HOME/.dolt/creds/<new-key-id>.jwk

# Restart pods that hold a stale dolt config in memory
kubectl rollout restart deploy/spire-dolt deploy/spire-steward -n spire
```

If the DoltHub account is correct but `403` persists, double-check
`dolthub.userName` in your overlay — the Dolt CLI's `user.name` MUST match
the DoltHub account that owns the JWK, or the remote rejects the push.

### Wizard pod stuck `Pending` (Anthropic API key invalid / rate-limited)

**Symptom:** `kubectl get pods -n spire` shows `<guild>-wizard-<bead>` pods
created by the operator but never reaching `Running`, OR they reach
`Running` and immediately exit with an authentication error visible in
`kubectl logs`.

**Likely cause:** the Anthropic API token configured in `SpireConfig.tokens`
is missing, malformed, or rate-limited. The operator places the pod
successfully but the agent container exits during `claude` startup.

**Diagnose:**

```bash
# Operator logs show the assignment but no progress
kubectl logs -n spire deploy/spire-operator | grep -i "wizard\|assigned\|spawn" | tail -20

# Examine the wizard pod itself
kubectl describe pod -n spire -l spire.awell.io/role=wizard | tail -40

# Pull the agent container logs (or last-terminated logs if it crashed)
WIZARD=$(kubectl get pod -n spire -l spire.awell.io/role=wizard -o name | head -1)
kubectl logs $WIZARD -n spire -c agent --previous 2>/dev/null \
  || kubectl logs $WIZARD -n spire -c agent

# Watch for these strings:
#   "401 Unauthorized"     -> bad/expired token
#   "429 Too Many Requests" or "rate_limit_exceeded" -> rate-limited
#   "invalid x-api-key"    -> wrong header / token format
#   "OAuth token expired"  -> need to re-run `claude setup-token`

# Verify the Secret holding the token resolves
kubectl get secret -n spire | grep -i 'anthropic\|token'
kubectl get secret <secret-name> -n spire -o jsonpath='{.data.api-key}' \
  | base64 -d | head -c 20
# Should start with "sk-ant-api03-" (classic) or "sk-ant-oat01-" (OAuth)
```

**Fix:**

```bash
# (a) Rotate the API key
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set anthropic.apiKey=sk-ant-api03-...

# (b) Switch to OAuth (Max plan) if you've been hitting rate limits
claude setup-token   # on a Max-subscribed account, copy the sk-ant-oat01- token
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set anthropic.oauthToken=sk-ant-oat01-...

# (c) Force the operator to re-create stuck pods after rotating the secret
kubectl delete pod -n spire -l spire.awell.io/role=wizard
```

If pods keep cycling through `429`, lower
`spec.maxConcurrent` on the WizardGuild to throttle wizard creation, or
upgrade your Anthropic plan.

### `/healthz` returns 502 (gateway routes but pods aren't Ready)

**Symptom:** `curl https://spire.<domain>/healthz` returns:

```
HTTP/2 502
```

…or a generic Google "backend service unavailable" page. The DNS resolves
and the TLS handshake succeeds, so the Gateway is in the path — the failure
is past the L7.

**Likely cause:** the gateway pods aren't passing their liveness/readiness
probes, so the load balancer's backend service has no healthy endpoints to
forward to.

**Diagnose:**

```bash
# Confirm gateway pods exist and check their Ready column
kubectl get pods -n spire -l app.kubernetes.io/name=spire-gateway

# Inspect probe failures on the pods
kubectl describe pod -n spire -l app.kubernetes.io/name=spire-gateway | tail -40

# Hit /healthz from inside a gateway pod to confirm the app itself is fine
kubectl exec -n spire deploy/spire-gateway -- \
  curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:8080/healthz
# Expect: 200

# If the pod-local hit succeeds but external 502s persist, check the
# BackendConfig / Service health-check config matches the app's path/port
kubectl get backendconfig -n spire -o yaml
kubectl describe service -n spire spire-gateway
```

**Fix:**

```bash
# (a) Probe failure inside the pod — read the gateway logs and fix the
# underlying error (most often a missing tower-attach secret or wrong
# BEADS_DIR; see "Wizard / steward / gateway pod can't read or write GCS"
# above for the WI variant).
kubectl logs deploy/spire-gateway -n spire -c gateway --tail=100

# (b) Health-check path mismatch — ensure the BackendConfig's
# healthCheckPath matches what the gateway serves
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set gateway.ingress.backendConfig.healthCheckPath=/healthz

# (c) After fixing, force a fresh probe cycle
kubectl rollout restart deploy/spire-gateway -n spire
kubectl rollout status deploy/spire-gateway -n spire --timeout=120s
```

If pods are Ready but the LB still returns 502, give Google ~60s to refresh
its backend health view, then retry. Persistent 502s with all pods Ready
usually indicate a port mismatch between the Service and the BackendConfig.

### `spire tower attach-cluster` returns 401 Unauthorized

**Symptom:** the laptop CLI run:

```
$ spire tower attach-cluster --url=https://spire.<domain> --token=<token>
Error: 401 Unauthorized
```

The gateway is reachable (no TLS or 502), but the bearer check rejects.

**Likely cause:** the token sent by the CLI does not match what the gateway
was configured with via `gateway.apiToken`. Either the laptop is using a
stale value, the chart was installed without setting one (defaults to
empty / dev-mode), or the gateway pod is still running with the old
ConfigMap.

**Diagnose:**

```bash
# Confirm what the gateway pod thinks the token is
kubectl get secret -n spire | grep -i gateway
kubectl get secret <gateway-secret> -n spire -o jsonpath='{.data.api-token}' | base64 -d

# Hit the gateway with the same header to isolate CLI vs server
curl -i -H "Authorization: Bearer <token>" https://spire.<domain>/api/v1/tower
# 200 -> token is fine, the CLI is using a stale value
# 401 -> token mismatch, fix at the cluster
```

**Fix:**

```bash
# Reset the gateway token via helm overlay
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set gateway.apiToken=<new-strong-token>
kubectl rollout restart deploy/spire-gateway -n spire

# Update the laptop with the new token
spire tower attach-cluster --url=https://spire.<domain> --token=<new-strong-token>
```

### Steward pod CrashLoopBackOff during init (`tower-attach`)

**Symptom:** `kubectl get pods -n spire` shows the steward pod stuck in
`Init:CrashLoopBackOff`. `kubectl describe` reveals the failing init
container is `tower-attach`.

**Likely cause:** the init container failed to clone the DoltHub tower
into the shared PVC, usually because the JWK is invalid or the named
DoltHub repo doesn't exist for the configured account.

**Diagnose:**

```bash
# Read the init container logs (each restart wipes them, so use --previous
# if the pod is in CrashLoopBackOff)
STEWARD=$(kubectl get pod -n spire -l app.kubernetes.io/name=spire-steward -o name | head -1)
kubectl logs $STEWARD -n spire -c tower-attach --previous

# Common log lines:
#   "fatal: repository '...' not found"        -> remoteUrl wrong
#   "fatal: authentication failed"             -> creds wrong (see Dolt push)
#   "fatal: ambiguous argument"                -> JWK matches but the
#                                                  account lacks read access
```

**Fix:** once the underlying DoltHub auth/URL is corrected, delete the
pod so the init container retries with fresh values:

```bash
kubectl delete pod -n spire $STEWARD
kubectl get pod -n spire -l app.kubernetes.io/name=spire-steward -w
```

### Beads aren't syncing back to DoltHub from the cluster

**Symptom:** wizards complete and close beads inside the cluster (visible
via `kubectl logs deploy/spire-steward`), but `spire pull` on the laptop
shows no movement; the dolt commit log on DoltHub has no new commits.

**Likely cause:** the dolt sync sidecar / syncer CronJob is disabled or
failing silently; OR the steward is committing locally but never pushing
because `dolthub.user`/`password` (the remotesapi user) is wrong.

**Diagnose:**

```bash
# Are syncs being attempted at all?
kubectl logs -n spire deploy/spire-steward -c sidecar | grep -iE "push|pull|sync"

# Or, if you enabled the syncer CronJob
kubectl get cronjob -n spire spire-syncer
kubectl get jobs -n spire | head
kubectl logs -n spire job/<latest-syncer-job>

# Confirm the dolt commit log inside the cluster has the changes
kubectl exec -n spire deploy/spire-dolt -- \
  dolt sql -q "SELECT message, committer, date FROM dolt_log LIMIT 5"
```

**Fix:** see "Dolt push fails with auth error" above for credential
problems. If the syncer is disabled, enable it:

```bash
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml \
  --set syncer.enabled=true \
  --set syncer.schedule="*/2 * * * *"
```

### First wizard pod runs but never produces any apprentice work

**Symptom:** the wizard pod is `Running`, `kubectl logs` shows it claimed
the bead, but no apprentice pods get created and the wizard eventually
exits without progress.

**Likely cause:** the WizardGuild's `prefixes` doesn't include the bead's
prefix, OR `bundleStore` is misconfigured so the apprentice bundle never
makes it back from the worktree to the cluster.

**Diagnose:**

```bash
# Confirm the guild's prefix list covers the bead
kubectl get wizardguild -n spire -o yaml | grep -A2 prefixes

# Check the wizard's recent log for bundle-store errors
WIZARD=$(kubectl get pod -n spire -l spire.awell.io/role=wizard -o name | head -1)
kubectl logs $WIZARD -n spire -c agent | grep -iE "bundle|gcs|store"

# If using GCS, list the bucket — the apprentice bundle should appear here
gsutil ls gs://<bucket>/<prefix>/
```

**Fix:**

```bash
# Update the guild's prefix list
kubectl edit wizardguild <name> -n spire   # add the bead's prefix to spec.prefixes

# Or, if bundle-store auth is the issue, verify Workload Identity (above)
# and confirm the bucket exists and the GSA has objectAdmin on it.
```

### Helm install partially failed; rerun shows "release: already exists"

**Symptom:** the first `helm install` errored out (network blip, validation
failure), and re-running gives:

```
Error: INSTALLATION FAILED: cannot re-use a name that is still in use
```

**Likely cause:** Helm tracked the failed install as a release in
`pending-install` state. It's recoverable — Helm wants you to either retry
the upgrade path or roll back.

**Diagnose:**

```bash
helm list -n spire --all
# Look for STATUS=pending-install or failed
helm history spire -n spire
```

**Fix:**

```bash
# Easiest: switch to upgrade --install (idempotent) for the retry
helm upgrade --install spire helm/spire -n spire -f values.gke.yaml ...

# If that still fails, uninstall and reinstall (PVCs survive by default)
helm uninstall spire -n spire
helm install spire helm/spire -n spire -f values.gke.yaml ...
```

If you want to clean state too, delete the PVCs after the uninstall — but
note that this wipes the on-cluster dolt database; only the DoltHub remote
preserves history past that point.
