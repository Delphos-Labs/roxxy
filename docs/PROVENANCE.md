# Provenance & Attestation for Airgap Deliveries

A Delphos airgap delivery is a **self-describing, independently verifiable**
artifact. This document explains the provenance model — what you receive, how
to verify it, and *why* the verification is sound — so that an operator can
establish, without trusting the delivery channel, that the bundle is:

1. **exactly what Delphos produced** (authenticity), and
2. **composed only of the components its SBOM enumerates** (transparency), and
3. **bit-for-bit intact** (integrity).

It is written to be shared with customers as the attestation reference for a
delivery.

---

## What you receive

| File | Purpose |
|------|---------|
| `delphos-airgap-<ts>.tar` | The airgap bundle — every container image and Helm chart, content-addressed. |
| `delivery.cdx.json` | CycloneDX SBOM enumerating every artifact in the bundle and its components (incl. the installer tools). |
| `delphos-airgap-<ts>.tar.sig`, `delivery.cdx.json.sig` | cosign (Sigstore) signatures over the two archives. |
| `cosign.pub` | The Delphos public key used to verify the signatures. |
| `delivery-manifest.{json,md}` | Human- and machine-readable inventory: refs, digests, licenses. |
| `SHA256SUMS` | Checksums of the delivered files. |

The **two signed archives** are the tarball and `delivery.cdx.json`. Signing
those two is sufficient: the tarball *is* the product, and the SBOM *describes*
the tarball (see below), so a valid signature on each closes the chain.

---

## The three pillars of trust

### 1. Integrity — content addressing
Every object in the bundle (image manifest, image layer, chart) is stored and
referenced by its **SHA-256 digest**. The digest *is* the cryptographic hash of
the content: a blob named `sha256:abc…` cannot contain anything but the bytes
that hash to `abc…`. Any modification changes the digest, which breaks the
manifest that references it. Integrity is therefore **intrinsic** — there is
nothing to "trust" beyond recomputing digests, which `roar` and OCI registries
do automatically on every pull.

### 2. Transparency — the SBOM describes *exactly* the shipped bytes
`delivery.cdx.json` is generated from the **same content-addressed objects**
that are serialized into the tarball, produced in a **single export pass**:

- At export, roxxy first **clears** the OCI-layout staging area, then walks
  every ref → manifest → blob exactly once, emitting **both**:
  - the tarball, and
  - a hardlinked OCI image-layout tree (`oci-layout` + `index.json` +
    `blobs/sha256/<digest>` **hardlinked** to the content-addressed blob store).
- The SBOM scanner (syft for images/Go binaries, cdxgen for charts) reads those
  layouts directly.

Two properties follow, **by construction** rather than by after-the-fact
comparison:

- **Exclusivity** — because the layout area is cleared before the walk, it
  contains *exactly* the refs serialized into this tarball: nothing stale from a
  prior export, nothing extra.
- **Identity** — because the layout's blobs are *hardlinks* to the same
  content-addressed objects the tarball serializes, the scanner reads the
  *identical bytes that ship*. The same `sha256:` digests appear in the tarball,
  the layout, and the SBOM.

There is therefore **no gap** between "what the SBOM says" and "what the bundle
contains." They are two views of one content-addressed source produced in one
pass; the digest is their shared, self-verifying identity.

### 3. Authenticity — cosign signatures
The tarball and the SBOM are each signed with Delphos's cosign (Sigstore)
private key. Verifying the signature with the distributed public key
(`cosign.pub`) proves the artifact was produced by Delphos and has not been
altered in transit. The private key is held only by Delphos; the public key is
distributed with every release.

---

## How to verify (operator steps)

```bash
# 1. AUTHENTICITY — verify both archives against the Delphos public key.
cosign verify-blob --key cosign.pub \
  --signature delphos-airgap-<ts>.tar.sig  delphos-airgap-<ts>.tar
cosign verify-blob --key cosign.pub \
  --signature delivery.cdx.json.sig        delivery.cdx.json

# 2. TRANSPARENCY — inspect exactly what is inside.
jq '.components | length'                          delivery.cdx.json
jq -r '.components[] | "\(.name)\t\(.version)"'     delivery.cdx.json
# delivery-manifest.md is the human-readable equivalent (refs, digests, licenses).

# 3. INTEGRITY — digests self-verify as the bundle loads into your registry.
roar push <your-registry> --from-tarball delphos-airgap-<ts>.tar --all
#   roar verifies every blob against its digest while streaming; a mismatch aborts.
#   Exit 0 = the whole bundle landed (benign tag conflicts included); exit 2 =
#   a ref failed or a manifest was unparseable — the registry is INCOMPLETE, do
#   not deploy. Gate your pipeline on this exit code.
```

A passing `cosign verify-blob` on **both** archives, plus a clean `roar push`,
is the complete chain:

> signed by Delphos → unaltered in transit → composed only of the SBOM's
> components → every component byte-verified on load.

---

## Why the SBOM provably matches the tarball (the key claim)

Most SBOM workflows generate the SBOM by scanning *a copy* of the artifact, then
hope the copy matched the shipped artifact. This model removes that gap:

- The export **wipes** `oci-layouts/` before the walk → the layout set is
  **exclusive** to this export.
- The tarball and the layouts are emitted from the **same walk** over the
  **same content-addressed store** → identical digests.
- The layouts use **hardlinks, not copies** → the scanner reads the identical
  inodes that ship.

The SBOM is thus a description of the tarball's exact content, anchored to the
same self-verifying digests — not a parallel artifact that could drift.

---

## Implementation (roxxy)

- **Export** clears `cache/archive/oci-layouts/` at start, then during the
  export walk that produces the tarball it also emits, per ref, an OCI layout:
  `oci-layout`, `index.json`, and `blobs/sha256/<digest>` hardlinked from
  `cache/docker-blobs/sha256/…`. Hardlinks are intra-volume, so this is
  metadata-only and shared base layers cost a single set of inodes.
- The **SBOM toolchain** mounts the roxxy cache volume read-only and scans
  `oci-dir:<cache>/archive/oci-layouts/<ref>` — no per-image extraction, no temp
  copies.
- **Signing** is performed over the resulting tarball + `delivery.cdx.json` with
  cosign; the public key ships with the delivery.

> Status: the content-addressed cache, tarball export, CycloneDX SBOM, and
> cosign signing are in place. The export-time hardlink-layout emission
> (above) is the mechanism that makes SBOM generation read-in-place **and**
> guarantees the layout↔tarball exclusivity the attestation relies on.
