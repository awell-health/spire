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
