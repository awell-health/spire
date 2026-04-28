# pkg/logartifact

Bead-scoped log artifact substrate. Owns:

1. **Domain types** — `Identity`, `Manifest`, `Visibility`, `Status`,
   `Stream`, `Role`. The deployment-independent address of an
   artifact's bytes.
2. **Two backends** — `LocalStore` (filesystem) and `GCSStore` (Google
   Cloud Storage), behind a single `Store` interface.
3. **Object key derivation** — `BuildObjectKey`, the pure function that
   turns an `Identity` into the canonical relative key shared by both
   backends. The exporter (spi-k1cnof) and gateway (spi-j3r694) compute
   the same key independently.
4. **Redaction + visibility** (spi-cmy90h) — `Visibility` enum,
   `pkg/logartifact/redact` patterns, upload-time redaction, and the
   `Render` helper that re-redacts on read.
5. **Manifest compaction** — `CompactManifests` for tower-side index
   bounding. Does NOT touch byte stores.

## Three-axis retention model

Cluster log retention has three independent owners. Don't conflate
them; this package only owns the third:

| Axis | Owner | Configuration | Default |
|------|-------|---------------|---------|
| **Cloud Logging** retention | GKE / operator | Cloud Logging
log-bucket retention setting | 30 days (GKE default) |
| **GCS artifact** retention | Bucket lifecycle policy | `gsutil lifecycle set lifecycle.json gs://<bucket>` | 90 days (Awell guidance) |
| **Tower manifest** retention | `CompactManifests` (this package) | `LogArtifactCompactionPolicy` in pkg/steward | PerBeadKeep=64, OlderThan=180d |

Cross-axis interactions:

- Cloud Logging retention is shorter than GCS to keep live-search
  cheap; durable replay reads through the gateway/manifest, not Cloud
  Logging.
- GCS retention is shorter than the manifest age cap so the manifest
  never points at deleted objects in the steady state. A render that
  hits a deleted object surfaces ErrNotFound; the gateway can then
  decide whether to fall back to a summary or report the gap.
- `CompactManifests` deletes manifest rows ONLY. The byte store keeps
  its objects until the lifecycle rule prunes them. There is no shared
  retention loop — by design.

## Visibility classes

Three classes, set at upload time, never auto-promoted:

- `engineer_only` (default) — raw bytes, untouched at upload.
  Forensic-replay fidelity. Render-time gate refuses non-engineer
  callers.
- `desktop_safe` — redacted before bytes hit the byte store, AND
  re-redacted on every read (defense in depth). Suitable for the
  desktop board and summary previews.
- `public` — redacted at upload + re-redacted on read. Reserved for
  surfaces shared outside the operating organization.

The upload path takes `Visibility` as a required argument. A
zero-value caller fails closed (insert with engineer_only); an empty
or invalid value at the call site fails fast.

## Redaction is hygiene, not a security boundary

`pkg/logartifact/redact` masks high-confidence credential shapes
(provider API keys, AWS/GCP creds, GitHub tokens, JWTs, Bearer
headers, generic api_key/password assignments, BEGIN PRIVATE KEY
blocks). It is a last-line filter, not a substitute for "don't log
secrets." A determined adversary can phrase a credential to escape;
patterns will miss tokens we haven't catalogued.

The right control is to keep secrets out of transcripts in the first
place. The cluster-install runbook
(`docs/cluster-install.md` § "Log retention, redaction, and visibility")
documents this explicitly.
