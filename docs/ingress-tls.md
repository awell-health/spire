# Gateway Ingress + TLS Runbook

Operator-facing guide for exposing the Spire gateway over HTTPS at a
real hostname. Values keys used below match the `gateway.ingress` schema
in `helm/spire/values.yaml`.

## Overview

Two strategies are supported; pick one.

- **A. GKE Ingress + Google-managed certificate** — preferred on GKE.
  Google provisions and renews the cert; no cert-manager required.
- **B. nginx-ingress + cert-manager** — for non-GKE clusters, or when
  you already run this stack.

The Ingress created by either strategy routes only `/api/v1/*` and
`/healthz` to the gateway Service. Webhooks are out of scope — see
[Webhooks (/sync)](#webhooks-sync).

---

## Strategy A: GKE + Google-managed cert

### 1. Reserve a global static IP

```bash
gcloud compute addresses create spire-gateway --global --project <proj>
```

Fetch the address:

```bash
gcloud compute addresses describe spire-gateway --global \
  --format='value(address)'
```

### 2. Create a DNS A record

Point `spire.<domain>` → the IP above. Allow ~5 min for propagation
before continuing.

### 3. Overlay values

Add to your overlay (e.g. `values.mycluster.yaml`):

```yaml
gateway:
  ingress:
    enabled: true
    className: gce
    host: spire.example.com
    managedCert:
      enabled: true
      name: spire-gateway-cert
    backendConfig:
      enabled: true
      http2: true
      healthCheckPath: /healthz
```

### 4. Install / upgrade

```bash
helm upgrade --install spire -n spire helm/spire \
  -f helm/spire/values.gke.yaml \
  -f values.mycluster.yaml
```

### 5. Wait for the cert to activate

Google provisioning typically takes 15–60 minutes after the Ingress
gets an external IP and DNS resolves.

```bash
kubectl describe managedcertificate spire-gateway-cert -n spire
```

`Status` must become `Active`.

### 6. Verify

```bash
curl -I https://spire.<domain>/healthz
```

Expect `200 OK` with a trusted (non-self-signed) certificate.

---

## Strategy B: nginx-ingress + cert-manager

### Prereqs

- nginx-ingress controller installed in the cluster — follow
  [kubernetes.github.io/ingress-nginx](https://kubernetes.github.io/ingress-nginx/deploy/).
- cert-manager installed with a `ClusterIssuer` (e.g.
  `letsencrypt-prod`) — follow
  [cert-manager.io/docs/installation](https://cert-manager.io/docs/installation/)
  and [cert-manager.io/docs/configuration/acme](https://cert-manager.io/docs/configuration/acme/).

### 1. Get the ingress-nginx external IP

```bash
kubectl get svc -n ingress-nginx
```

Record the `EXTERNAL-IP` of the controller Service.

### 2. Create a DNS A record

Point `spire.<domain>` → that IP.

### 3. Overlay values

```yaml
gateway:
  ingress:
    enabled: true
    className: nginx
    host: spire.example.com
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt-prod
    tls:
      enabled: true
      secretName: spire-gateway-tls
```

### 4. Install / upgrade

```bash
helm upgrade --install spire -n spire helm/spire \
  -f values.mycluster.yaml
```

### 5. Wait for the cert

cert-manager solves the ACME challenge and writes the cert into
`spire-gateway-tls`:

```bash
kubectl describe certificate spire-gateway-tls -n spire
```

Wait for `Ready: True`.

### 6. Verify

```bash
curl -I https://spire.<domain>/healthz
```

---

## TLS renewal

- **Strategy A:** Google renews the managed cert automatically. No
  operator action required.
- **Strategy B:** cert-manager renews automatically, roughly 30 days
  before expiry. If you want to confirm, watch `kubectl describe
  certificate spire-gateway-tls -n spire` for renewal events.

---

## Webhooks (/sync)

The Ingress created here **only** routes `/api/v1/*` and `/healthz`.

The webhook receiver (`POST /sync`) is not routed by this Ingress — it
keeps its own port/mechanism. Exposing webhooks over HTTPS is out of
scope for v1; use a separate Ingress or host if you need that.

---

## GKE Autopilot first-install gotchas

A fresh GKE Autopilot cluster needs two extra Ingress fields that classic
GKE does not. The chart now emits both unconditionally; this section
documents what they fix and the manual workarounds for older chart
versions.

### Legacy `kubernetes.io/ingress.class` annotation

**Symptom:** the Ingress sits with an empty `status.loadBalancer`,
no GCP forwarding rules or NEGs are created, and `kubectl describe
ingress` shows no events at all (not even sync attempts).

**Cause:** Autopilot does not pre-populate an `IngressClass` CR for the
legacy `gce` name (`kubectl get ingressclass` returns empty). Without an
IngressClass CR, the GLBC controller relies on the legacy
`kubernetes.io/ingress.class` annotation. `spec.ingressClassName: gce`
alone is not enough.

**Fix:** the chart emits both APIs side-by-side whenever
`gateway.ingress.className` is set. Setting both is harmless on classic
GKE and on other ingress controllers (nginx / traefik) — controllers
that use IngressClass CRs ignore the annotation when it doesn't match.

**Workaround for older chart versions:**

```bash
kubectl annotate ingress spire-gateway -n spire \
  kubernetes.io/ingress.class=gce
```

### Explicit `spec.defaultBackend`

**Symptom:** even after the legacy annotation is in place, the Ingress
still does not provision a load balancer. `kubectl describe ingress`
eventually surfaces a controller error referencing a missing
`k8s1-...kube-system-default-http-backend-80-...` NEG.

**Cause:** Autopilot does not create the
`kube-system/default-http-backend` Service or its NEG. GLBC looks for
that NEG when no `spec.defaultBackend` is set on the Ingress, and the
sync stalls when it cannot find one.

**Fix:** the chart emits `spec.defaultBackend` pointing at the gateway
Service unconditionally (gated on `gateway.ingress.defaultBackend.enabled`,
default `true`). The gateway already serves `/healthz` on the named
port the LB hits, so this is harmless on classic GKE / nginx /
minikube.

**Workaround for older chart versions:**

```bash
kubectl patch ingress spire-gateway -n spire --type=merge -p \
  '{"spec":{"defaultBackend":{"service":{"name":"spire-gateway","port":{"name":"api"}}}}}'
```

**Upgrade note:** if you previously hand-patched a different
`spec.defaultBackend` onto the Ingress, `helm upgrade` to the chart
version that includes this fix will overwrite it. Disable the
chart-emitted default backend with
`--set gateway.ingress.defaultBackend.enabled=false` and re-apply your
patch if needed.

---

## Troubleshooting

### ManagedCertificate stuck in `Provisioning`

1. Verify DNS resolves:

    ```bash
    dig spire.<domain>
    ```

2. Verify the Ingress has an external IP assigned:

    ```bash
    kubectl get ingress -n spire
    ```

   The managed cert cannot activate until the Ingress has an IP **and**
   DNS points at it.

### 404 from the Ingress

Check that the Ingress backend Service name matches what was actually
deployed:

```bash
kubectl describe ingress <release>-spire-gateway -n spire
```

The backend should be the gateway Service (e.g.
`<release>-spire-gateway`).

### Healthcheck failing

Confirm `/healthz` returns 200 from inside the gateway pod:

```bash
kubectl exec -n spire <gateway-pod> -- curl -s localhost:8080/healthz
```

If this fails, the pod itself is unhealthy — fix that before debugging
the Ingress path.
