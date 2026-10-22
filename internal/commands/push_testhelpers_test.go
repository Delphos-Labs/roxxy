package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// ---------------------------------------------------------------------------
// Synthetic roxxy-cache tarball builder
// ---------------------------------------------------------------------------

// syntheticBlob is one content-addressed blob destined for a tarball: its bytes
// plus the sha256:<hex> digest derived from them.
type syntheticBlob struct {
	digest string
	data   []byte
}

func makeBlob(content string) syntheticBlob {
	sum := sha256.Sum256([]byte(content))
	return syntheticBlob{
		digest: fmt.Sprintf("sha256:%x", sum),
		data:   []byte(content),
	}
}

// syntheticImage is a single docker image entry: a repo:tag, a config blob, the
// ordered layer blobs, and the manifest JSON that references them. The manifest
// is a schemaVersion-2 docker manifest with sizes filled in from the blob bytes.
type syntheticImage struct {
	registryID string // normalized registry host, e.g. "docker.io"
	repo       string // logical repo, e.g. "library/redis"
	tag        string
	config     syntheticBlob
	layers     []syntheticBlob
	manifest   []byte // computed by buildManifest
}

// buildManifest renders the docker manifest v2 JSON referencing the config and
// layers and stores it on the image. Sizes come from the actual blob bytes so a
// downstream content-equivalence check sees consistent descriptors.
func (img *syntheticImage) buildManifest() {
	type desc struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
	}
	m := struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Config        desc   `json:"config"`
		Layers        []desc `json:"layers"`
	}{
		SchemaVersion: 2,
		MediaType:     dockerManifestV2MediaType,
		Config: desc{
			MediaType: dockerContainerConfigMediaType,
			Digest:    img.config.digest,
			Size:      len(img.config.data),
		},
	}
	for _, l := range img.layers {
		m.Layers = append(m.Layers, desc{
			MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip",
			Digest:    l.digest,
			Size:      len(l.data),
		})
	}
	// Compact (no indent) keeps the manifest bytes deterministic and small;
	// the exact byte form is what the manifest digest is computed over.
	data, _ := json.Marshal(m)
	img.manifest = data
}

// shardedBlobPath mirrors registry getBlobPath / getBlobPathForTarball: blobs
// live at docker-blobs/sha256/<ab>/<cd>/sha256:<hash>. OpenBlob/BlobSizes only
// resolve this layout, so the builder must use it verbatim.
func shardedBlobPath(digest string) string {
	hashPart := strings.TrimPrefix(digest, "sha256:")
	return fmt.Sprintf("docker-blobs/sha256/%s/%s/%s", hashPart[0:2], hashPart[2:4], digest)
}

// manifestPathFor mirrors FindManifestByRepoTag's layout:
// docker-manifests/<registryID>/<safeRepo>/<tag>.
func manifestPathFor(img syntheticImage) string {
	return fmt.Sprintf("docker-manifests/%s/%s/%s", img.registryID, util.SafeRepoName(img.repo), img.tag)
}

// writeSyntheticTarball writes a roxxy-cache tarball on disk containing the
// given images plus any extra raw entries (used to inject an unparseable
// manifest path). It deduplicates blobs by digest across images so a shared
// blob is written exactly once — matching real cache layout — and appends the
// gzipped .roxxy-index.json block at the end so the seek-based readers
// (OpenBlob/BlobSizes/FindManifestByRepoTag) resolve via offsets.
//
// Returns the tarball path. The index is version 2 (block numbers), matching
// what the production exporter writes and what buildSectionIndex expects.
func writeSyntheticTarball(t *testing.T, images []syntheticImage, extraEntries map[string]string) string {
	t.Helper()

	tarballPath := filepath.Join(t.TempDir(), "synthetic.tar")
	file, err := os.Create(tarballPath)
	if err != nil {
		t.Fatalf("create synthetic tarball: %v", err)
	}
	tw := tar.NewWriter(file)

	offset := int64(0)
	blockOffsets := make(map[string]int32)

	writeEntry := func(name string, content []byte) {
		if offset%512 != 0 {
			offset = ((offset + 511) / 512) * 512
		}
		blockOffsets[name] = int32(offset / 512)
		hdr := &tar.Header{
			Name:     name,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
			Mode:     0644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		offset += 512
		n, err := tw.Write(content)
		if err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
		offset += int64(n)
		if pad := (512 - (int64(n) % 512)) % 512; pad > 0 {
			offset += pad
		}
	}

	// Manifests first, then deduplicated blobs — same section ordering the real
	// exporter uses (docker-manifests/ then docker-blobs/).
	for i := range images {
		images[i].buildManifest()
		writeEntry(manifestPathFor(images[i]), images[i].manifest)
	}
	for name, content := range extraEntries {
		writeEntry(name, []byte(content))
	}
	seenBlob := make(map[string]bool)
	for _, img := range images {
		for _, b := range append([]syntheticBlob{img.config}, img.layers...) {
			if seenBlob[b.digest] {
				continue
			}
			seenBlob[b.digest] = true
			writeEntry(shardedBlobPath(b.digest), b.data)
		}
	}

	// Append the version-2 index (gzipped) so buildSectionIndex finds offsets.
	if offset%512 != 0 {
		offset = ((offset + 511) / 512) * 512
	}
	indexJSON, err := json.Marshal(map[string]any{
		"version":      2,
		"file_offsets": blockOffsets,
	})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(indexJSON); err != nil {
		t.Fatalf("gzip index: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	idxHdr := &tar.Header{
		Name:     ".roxxy-index.json.gz",
		Size:     int64(gz.Len()),
		Typeflag: tar.TypeReg,
		Mode:     0644,
	}
	if err := tw.WriteHeader(idxHdr); err != nil {
		t.Fatalf("write index header: %v", err)
	}
	if _, err := tw.Write(gz.Bytes()); err != nil {
		t.Fatalf("write index body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close tarball file: %v", err)
	}
	return tarballPath
}

// twoImagesSharingABlob builds two single-layer images that share one layer
// digest and each carry a distinct config + a distinct layer, so a push
// exercises cross-repo dedup/mount on the shared blob. Returns the images and
// the shared layer's digest for assertions.
func twoImagesSharingABlob() (images []syntheticImage, sharedDigest string) {
	shared := makeBlob("shared-layer-bytes-common-to-both-images")
	imgA := syntheticImage{
		registryID: "docker.io",
		repo:       "library/alpha",
		tag:        "1.0",
		config:     makeBlob("config-for-alpha"),
		layers:     []syntheticBlob{shared, makeBlob("alpha-unique-layer")},
	}
	imgB := syntheticImage{
		registryID: "docker.io",
		repo:       "library/beta",
		tag:        "2.0",
		config:     makeBlob("config-for-beta"),
		layers:     []syntheticBlob{shared, makeBlob("beta-unique-layer")},
	}
	return []syntheticImage{imgA, imgB}, shared.digest
}

// ---------------------------------------------------------------------------
// Fake OCI distribution registry (push side)
// ---------------------------------------------------------------------------

// fakeRegistry implements enough of the OCI distribution push API for the push
// coordinator: blob existence HEAD, upload-session POST/PATCH/PUT, cross-repo
// mount, and manifest PUT/HEAD/GET. All mutating state is mutex-guarded so the
// recorded counts are race-free under `go test -race`.
type fakeRegistry struct {
	server *httptest.Server

	mu sync.Mutex
	// blobUploadPUTs counts real blob commit PUTs per "<repo>|<digest>". A
	// cross-repo mount or an already-present skip records NOTHING here, so this
	// is the ground truth for "was this blob's body actually uploaded".
	blobUploadPUTs map[string]int
	// haveBlob records which (repo,digest) pairs the registry holds, populated
	// by completed uploads and successful mounts. Drives HEAD and mount.
	haveBlob map[string]bool
	// globalBlob records every digest the registry holds in ANY repo, so a
	// mount from a known source repo can succeed.
	globalBlob map[string]bool
	// manifestPUTs counts manifest PUTs per repo.
	manifestPUTs map[string]int
	// uploads tracks open upload sessions: session-id -> accumulated body.
	uploads map[string]*bytes.Buffer
	// failDigestUpload, when set, makes the commit PUT for that digest return
	// 500 — used to exercise the error-surfacing path.
	failDigestUpload string
	// failManifestRepo, when set, makes the manifest PUT for that repo return
	// 500 — exercises the failures (vs conflicts) partition in pushOutcome.
	failManifestRepo string
	nextUpload       int
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	fr := &fakeRegistry{
		blobUploadPUTs: make(map[string]int),
		haveBlob:       make(map[string]bool),
		globalBlob:     make(map[string]bool),
		manifestPUTs:   make(map[string]int),
		uploads:        make(map[string]*bytes.Buffer),
	}
	fr.server = httptest.NewServer(http.HandlerFunc(fr.handle))
	t.Cleanup(fr.server.Close)
	return fr
}

func (fr *fakeRegistry) URL() string { return fr.server.URL }

// failUpload forces the commit PUT for digest to return 500.
func (fr *fakeRegistry) failUpload(digest string) {
	fr.mu.Lock()
	fr.failDigestUpload = digest
	fr.mu.Unlock()
}

// failManifest forces the manifest PUT for repo to return 500 — drives the
// failures partition in pushOutcome (distinct from a benign tag conflict).
func (fr *fakeRegistry) failManifest(repo string) {
	fr.mu.Lock()
	fr.failManifestRepo = repo
	fr.mu.Unlock()
}

// totalUploads returns, per digest (across all repos), the real commit PUT
// count — the exactly-once assertion uses this.
func (fr *fakeRegistry) totalUploads(digest string) int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	total := 0
	for k, v := range fr.blobUploadPUTs {
		if strings.HasSuffix(k, "|"+digest) {
			total += v
		}
	}
	return total
}

func (fr *fakeRegistry) manifestPushCount(repo string) int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.manifestPUTs[repo]
}

// repoFromPath extracts the <name> in /v2/<name>/{blobs,manifests}/...
// supporting multi-segment repo names like library/alpha.
func repoFromPath(path, kind string) string {
	rest := strings.TrimPrefix(path, "/v2/")
	idx := strings.Index(rest, "/"+kind+"/")
	if idx < 0 {
		// uploads path: /v2/<name>/blobs/uploads/
		idx = strings.Index(rest, "/blobs/uploads")
		if idx < 0 {
			return ""
		}
	}
	return rest[:idx]
}

func (fr *fakeRegistry) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/blobs/uploads/"):
		fr.handleUpload(w, r)
	case strings.Contains(r.URL.Path, "/blobs/"):
		fr.handleBlob(w, r)
	case strings.Contains(r.URL.Path, "/manifests/"):
		fr.handleManifest(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleBlob serves HEAD/GET on a stored blob.
func (fr *fakeRegistry) handleBlob(w http.ResponseWriter, r *http.Request) {
	repo := repoFromPath(r.URL.Path, "blobs")
	digest := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	fr.mu.Lock()
	have := fr.haveBlob[repo+"|"+digest]
	fr.mu.Unlock()
	if have {
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// handleUpload covers POST (init or mount), PATCH (chunk), and PUT (commit).
func (fr *fakeRegistry) handleUpload(w http.ResponseWriter, r *http.Request) {
	repo := repoFromPath(r.URL.Path, "blobs")
	switch r.Method {
	case http.MethodPost:
		// Cross-repo mount attempt?
		if mount := r.URL.Query().Get("mount"); mount != "" {
			from := r.URL.Query().Get("from")
			fr.mu.Lock()
			known := fr.globalBlob[mount] && from != ""
			if known {
				fr.haveBlob[repo+"|"+mount] = true
			}
			fr.mu.Unlock()
			if known {
				w.Header().Set("Docker-Content-Digest", mount)
				w.WriteHeader(http.StatusCreated) // 201: mounted
				return
			}
			// Fall through to a normal upload session (202).
		}
		fr.mu.Lock()
		fr.nextUpload++
		id := fmt.Sprintf("upl-%d", fr.nextUpload)
		fr.uploads[id] = &bytes.Buffer{}
		fr.mu.Unlock()
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, id))
		w.Header().Set("Range", "0-0")
		w.WriteHeader(http.StatusAccepted)

	case http.MethodPatch:
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, _ := io.ReadAll(r.Body)
		fr.mu.Lock()
		buf := fr.uploads[id]
		var wrote int
		if buf != nil {
			wrote = buf.Len()
			buf.Write(body)
		}
		fr.mu.Unlock()
		if buf == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, id))
		w.Header().Set("Range", fmt.Sprintf("0-%d", wrote+len(body)-1))
		w.WriteHeader(http.StatusAccepted)

	case http.MethodPut:
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		digest := r.URL.Query().Get("digest")
		body, _ := io.ReadAll(r.Body)
		fr.mu.Lock()
		if buf := fr.uploads[id]; buf != nil {
			buf.Write(body) // monolithic path streams the whole body on PUT
			delete(fr.uploads, id)
		}
		shouldFail := fr.failDigestUpload != "" && fr.failDigestUpload == digest
		if !shouldFail {
			fr.blobUploadPUTs[repo+"|"+digest]++
			fr.haveBlob[repo+"|"+digest] = true
			fr.globalBlob[digest] = true
		}
		fr.mu.Unlock()
		if shouldFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleManifest covers HEAD/GET (existence/fetch) and PUT (store).
func (fr *fakeRegistry) handleManifest(w http.ResponseWriter, r *http.Request) {
	repo := repoFromPath(r.URL.Path, "manifests")
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		// Fresh registry: nothing stored yet, so precheck sees no conflict.
		w.WriteHeader(http.StatusNotFound)
	case http.MethodPut:
		fr.mu.Lock()
		fr.manifestPUTs[repo]++
		shouldFail := fr.failManifestRepo != "" && fr.failManifestRepo == repo
		fr.mu.Unlock()
		if shouldFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// openSynthTarball is a tiny convenience wrapper used by the tests.
func openSynthTarball(t *testing.T, path string) *cli.TarballReader {
	t.Helper()
	reader, err := cli.OpenTarball(path)
	if err != nil {
		t.Fatalf("open synthetic tarball: %v", err)
	}
	t.Cleanup(func() { util.Logerr(reader.Close()) })
	return reader
}
