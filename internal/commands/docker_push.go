package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// DefaultPushWorkers is the default blob-upload concurrency. Sized at
// runtime.NumCPU()+1: each worker spends most of its time blocked on a
// network round-trip, but the sha256 verification + read-ahead it drives
// is CPU-bound enough that NCPU gives every core useful work, plus one
// extra to absorb the occasional disk/seek stall without leaving the
// uplink idle. Override with --workers.
var DefaultPushWorkers = runtime.NumCPU() + 1

// pushFromTarball replays a single ref's manifest tree from a roxxy tarball
// to a registry, streaming each blob straight onto the wire. The cached
// manifest's media type is preserved, so this works for any OCI artifact
// roxxy has cached: docker images, OCI images, helm charts, etc.
//
// If `platform` is non-empty and the tarball ref resolves to a multi-arch
// index, the matching child manifest is selected and pushed as a single-arch
// image under the same tag.
//
// Authentication uses the docker config (credential helpers, then auths
// block, then anonymous), so `docker login` /
// `aws ecr get-login-password | docker login` "just works".
func pushFromTarball(_ Config, registry, ref, tarballPath, platform string, workers int, coerceDocker bool) {
	repoName, tag, err := parseImageSpec(ref)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	platformSuffix := ""
	if platform != "" {
		platformSuffix = fmt.Sprintf(" [%s]", platform)
	}
	fmt.Printf("Pushing %s:%s%s from tarball %s to %s...\n", repoName, tag, platformSuffix, tarballPath, registry)

	reader, err := cli.OpenTarball(tarballPath)
	if err != nil {
		fmt.Printf("Error opening tarball: %v\n", err)
		os.Exit(1)
	}
	defer util.DeferClose(reader)

	manifestData, _, blobDigests, err := reader.FindManifestByRepoTag(repoName, tag, platform)
	if err != nil {
		fmt.Printf("Error finding manifest: %v\n", err)
		os.Exit(1)
	}

	r := &pushRef{
		repoName:       repoName,
		tag:            tag,
		manifestData:   manifestData,
		manifestDigest: digestOfBytes(manifestData),
		blobDigests:    blobDigests,
	}
	fmt.Printf("Found manifest %s with %d blobs\n", r.manifestDigest[:19], len(blobDigests))

	rc, err := newRegistryClient(registry, workers)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	sizes, err := reader.BlobSizes()
	if err != nil {
		fmt.Printf("Error reading blob sizes: %v\n", err)
		os.Exit(1)
	}
	uniqueBlobs := make(map[string]struct{}, len(blobDigests))
	for _, d := range blobDigests {
		uniqueBlobs[d] = struct{}{}
	}
	progress := newProgressReporter(len(uniqueBlobs))
	progress.start()
	coord := newPushCoordinator(rc, reader, workers, progress, sizes, coerceDocker)
	outcome, err := coord.Run([]*pushRef{r})
	progress.stop()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	if len(outcome.failures) > 0 {
		// Single-ref push: a genuine push failure means the ref did not land.
		fmt.Printf("Error: failed to push %s:%s: %s\n",
			outcome.failures[0].repoName, outcome.failures[0].tag, outcome.failures[0].failReason)
		os.Exit(1)
	}
	if len(outcome.conflicts) > 0 {
		// Single-ref push: a conflict on the named tag is the user's
		// surprise; surface it as an error rather than a silent skip.
		fmt.Printf("Error: %s:%s already points at %s in registry; refusing to overwrite\n",
			outcome.conflicts[0].repoName, outcome.conflicts[0].tag, outcome.conflicts[0].conflictDgst)
		os.Exit(1)
	}
	fmt.Printf("Successfully pushed %s:%s to %s\n", repoName, tag, registry)
}

func digestOfBytes(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// manifestsEquivalent fetches the existing manifest at repoName:tag and
// compares its config + layer descriptors against ours. Two manifests with
// the same config digest, same layer digests in order, and same mediaType
// describe the same image — only the byte-level encoding differs.
func manifestsEquivalent(rc *registryClient, repoName, tag string, ourData []byte) (bool, error) {
	theirData, err := rc.fetchManifest(repoName, tag)
	if err != nil {
		return false, err
	}
	ours, err := parseManifestDescriptors(ourData)
	if err != nil {
		return false, err
	}
	theirs, err := parseManifestDescriptors(theirData)
	if err != nil {
		return false, err
	}
	if ours.MediaType != theirs.MediaType {
		return false, nil
	}
	if ours.Config != theirs.Config {
		return false, nil
	}
	if len(ours.Layers) != len(theirs.Layers) {
		return false, nil
	}
	for i := range ours.Layers {
		if ours.Layers[i] != theirs.Layers[i] {
			return false, nil
		}
	}
	return true, nil
}

type manifestDescriptors struct {
	MediaType string
	Config    string   // "<mediaType>|<digest>|<size>"
	Layers    []string // same shape
}

func parseManifestDescriptors(data []byte) (manifestDescriptors, error) {
	var m struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return manifestDescriptors{}, err
	}
	d := manifestDescriptors{
		MediaType: m.MediaType,
		Config:    fmt.Sprintf("%s|%s|%d", m.Config.MediaType, m.Config.Digest, m.Config.Size),
	}
	for _, l := range m.Layers {
		d.Layers = append(d.Layers, fmt.Sprintf("%s|%s|%d", l.MediaType, l.Digest, l.Size))
	}
	return d, nil
}

// parseImageSpec splits "repo:tag" and normalizes a bare repo into "library/<repo>"
// to match the Docker Hub default.
func parseImageSpec(spec string) (repo, tag string, err error) {
	if !strings.Contains(spec, ":") {
		return "", "", fmt.Errorf("image spec must include tag: %s (expected repo:tag)", spec)
	}
	parts := strings.SplitN(spec, ":", 2)
	repo = parts[0]
	tag = parts[1]
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return repo, tag, nil
}

// Manifest and config media-type constants. We list these explicitly so the
// detection logic in detectManifestMediaType reads as a switch over known
// artifact shapes rather than a tangle of string literals.
const (
	dockerManifestV2MediaType = "application/vnd.docker.distribution.manifest.v2+json"
	ociImageManifestMediaType = "application/vnd.oci.image.manifest.v1+json"

	dockerContainerConfigMediaType = "application/vnd.docker.container.image.v1+json"
	ociImageConfigMediaType        = "application/vnd.oci.image.config.v1+json"
	// helmConfigMediaType lives in helm_push.go.
)

// detectManifestMediaType picks the manifest mediatype to PUT under for a
// given cached manifest. It prefers an explicit top-level `mediaType` field
// when present. When it's missing — common for OCI manifests that were
// referenced from an index by mediaType-on-the-descriptor and never
// re-stamped at the leaf — it derives the type from `config.mediaType` so a
// helm chart or OCI image isn't silently downgraded to a docker manifest.
//
// If coerceDocker is set, we honor the caller's request to force a docker
// manifest mediatype except when the artifact is a helm chart, where doing
// so produces an unusable result. Helm charts and helm configs always
// refuse coercion.
func detectManifestMediaType(manifestData []byte, coerceDocker bool) (string, error) {
	var m struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
	}
	_ = json.Unmarshal(manifestData, &m) // best-effort; bad JSON falls through to fallback

	isHelm := m.Config.MediaType == helmConfigMediaType

	if coerceDocker {
		if isHelm {
			return "", fmt.Errorf("refusing to coerce a helm chart manifest to docker mediatype (config: %s); helm charts must be pushed as OCI artifacts", m.Config.MediaType)
		}
		return dockerManifestV2MediaType, nil
	}

	if m.MediaType != "" {
		return m.MediaType, nil
	}

	switch m.Config.MediaType {
	case helmConfigMediaType, ociImageConfigMediaType:
		return ociImageManifestMediaType, nil
	case dockerContainerConfigMediaType:
		return dockerManifestV2MediaType, nil
	case "":
		// No mediaType anywhere — preserve the historical fallback for
		// docker v1-shaped manifests that have neither field.
		return dockerManifestV2MediaType, nil
	default:
		// Unknown config type — assume OCI manifest mediatype since it's
		// the broader spec; docker manifest v2 has stricter shape rules.
		return ociImageManifestMediaType, nil
	}
}

// tickingReader credits bytes to a progressReporter on every Read so the
// status display ticks live during a long single-blob upload, instead of
// jumping by `size` only at completion.
type tickingReader struct {
	r io.Reader
	p *progressReporter
}

func (tr *tickingReader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	if n > 0 {
		tr.p.addBytes(int64(n))
	}
	return n, err
}

// verifyingReader streams bytes through a sha256 hasher so we can compare the
// computed digest against the one we promised the registry. Catches a corrupt
// tarball before the wrong content reaches the wire — well, technically the
// registry will reject a digest mismatch on its own, but we get a clearer
// error and log the failing blob ourselves.
type verifyingReader struct {
	r        io.Reader
	h        hash.Hash
	expected string
}

func (vr *verifyingReader) Read(p []byte) (int, error) {
	n, err := vr.r.Read(p)
	if n > 0 {
		_, _ = vr.h.Write(p[:n])
	}
	return n, err
}

func (vr *verifyingReader) verify() error {
	got := "sha256:" + hex.EncodeToString(vr.h.Sum(nil))
	if got != vr.expected {
		return fmt.Errorf("digest mismatch: tarball blob computed %s, expected %s", got, vr.expected)
	}
	return nil
}

// formatBytes formats bytes as human-readable string
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
