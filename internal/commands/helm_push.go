package commands

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/util"
	"gopkg.in/yaml.v3"
)

const (
	helmConfigMediaType = "application/vnd.cncf.helm.config.v1+json"
	helmChartMediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	ociManifestType     = "application/vnd.oci.image.manifest.v1+json"
)

// chartMetadata is the subset of helm Chart.yaml we need to build the OCI
// config blob. Helm's contract is that name+version are mandatory; everything
// else flows through verbatim from Chart.yaml.
type chartMetadata struct {
	Name        string   `yaml:"name" json:"name"`
	Version     string   `yaml:"version" json:"version"`
	APIVersion  string   `yaml:"apiVersion,omitempty" json:"apiVersion,omitempty"`
	AppVersion  string   `yaml:"appVersion,omitempty" json:"appVersion,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Type        string   `yaml:"type,omitempty" json:"type,omitempty"`
	Keywords    []string `yaml:"keywords,omitempty" json:"keywords,omitempty"`
}

// PushHelmChart uploads a packaged helm chart (.tgz) to an OCI registry.
//
// `registry` is the registry hostname (e.g. "123.dkr.ecr.us-east-1.amazonaws.com").
// `repoBase` is the namespace under which the chart will live; the final
// repository becomes "<repoBase>/<chart-name>" per helm convention. Pass the
// empty string to push at the registry root.
//
// The chart name and version are read from Chart.yaml inside the tarball; the
// chart .tgz is streamed straight onto the wire without buffering.
func PushHelmChart(_ Config, registry, chartTgzPath, repoBase string) {
	meta, err := readChartMetadata(chartTgzPath)
	if err != nil {
		fmt.Printf("Error reading chart metadata: %v\n", err)
		os.Exit(1)
	}
	if meta.Name == "" || meta.Version == "" {
		fmt.Printf("Error: Chart.yaml must specify name and version\n")
		os.Exit(1)
	}

	repoName := meta.Name
	if repoBase != "" {
		repoName = strings.TrimSuffix(repoBase, "/") + "/" + meta.Name
	}

	fmt.Printf("Pushing helm chart %s:%s from %s to %s/%s...\n",
		meta.Name, meta.Version, chartTgzPath, registry, repoName)

	// Config blob: the chart metadata, serialized as JSON. Helm reads this
	// during pull to populate Chart.yaml-equivalent fields without unpacking
	// the tarball.
	configBytes, err := json.Marshal(meta)
	if err != nil {
		fmt.Printf("Error marshaling chart config: %v\n", err)
		os.Exit(1)
	}
	configDigest, configSize := digestAndSize(configBytes)

	chartInfo, err := os.Stat(chartTgzPath)
	if err != nil {
		fmt.Printf("Error stat'ing chart: %v\n", err)
		os.Exit(1)
	}
	chartSize := chartInfo.Size()

	chartDigest, err := digestFile(chartTgzPath)
	if err != nil {
		fmt.Printf("Error digesting chart: %v\n", err)
		os.Exit(1)
	}

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociManifestType,
		"config": map[string]any{
			"mediaType": helmConfigMediaType,
			"digest":    configDigest,
			"size":      configSize,
		},
		"layers": []map[string]any{
			{
				"mediaType": helmChartMediaType,
				"digest":    chartDigest,
				"size":      chartSize,
			},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		fmt.Printf("Error marshaling manifest: %v\n", err)
		os.Exit(1)
	}

	rc, err := newRegistryClient(registry, 4)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// Push config blob (tiny — buffered is fine)
	if err := pushBytesIfMissing(rc, repoName, configDigest, "config", configBytes); err != nil {
		fmt.Printf("Error pushing config blob: %v\n", err)
		os.Exit(1)
	}

	// Push chart blob (large — streamed from disk)
	exists, err := rc.blobExists(repoName, chartDigest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: HEAD %s failed: %v (will attempt upload)\n", chartDigest, err)
		exists = false
	}
	if exists {
		fmt.Printf("Chart blob %s already in registry, skipping\n", chartDigest[:12])
	} else {
		fmt.Printf("Pushing chart blob %s (%s)...", chartDigest[:12], formatBytes(chartSize))
		f, err := os.Open(chartTgzPath)
		if err != nil {
			fmt.Printf("\nError opening chart: %v\n", err)
			os.Exit(1)
		}
		// Verify the bytes we actually stream hash to chartDigest, mirroring
		// realUpload's verifyingReader on the tarball push path — catches a
		// chart modified between digestFile and the upload, or a corrupt read,
		// before the wrong content lands under the promised digest.
		vr := &verifyingReader{r: f, h: sha256.New(), expected: chartDigest}
		err = rc.pushBlob(rc.clientFor(0), repoName, chartDigest, chartSize, vr)
		util.Logerr(f.Close())
		if err == nil {
			err = vr.verify()
		}
		if err != nil {
			fmt.Printf("\nError: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf(" done\n")
	}

	fmt.Printf("Pushing manifest...")
	if err := rc.pushManifest(repoName, meta.Version, ociManifestType, manifestBytes); err != nil {
		fmt.Printf("\nError: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf(" done\n")
	fmt.Printf("Successfully pushed %s:%s to %s/%s\n", meta.Name, meta.Version, registry, repoName)
}

func pushBytesIfMissing(rc *registryClient, repoName, digest, label string, data []byte) error {
	exists, err := rc.blobExists(repoName, digest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: HEAD %s failed: %v (will attempt upload)\n", digest, err)
		exists = false
	}
	if exists {
		fmt.Printf("%s blob %s already in registry, skipping\n", label, digest[:12])
		return nil
	}
	fmt.Printf("Pushing %s blob %s (%s)...", label, digest[:12], formatBytes(int64(len(data))))
	vr := &verifyingReader{r: strings.NewReader(string(data)), h: sha256.New(), expected: digest}
	if err := rc.pushBlob(rc.clientFor(0), repoName, digest, int64(len(data)), vr); err != nil {
		fmt.Println()
		return err
	}
	if err := vr.verify(); err != nil {
		fmt.Println()
		return err
	}
	fmt.Printf(" done\n")
	return nil
}

// readChartMetadata extracts Chart.yaml from a packaged chart .tgz.
// Helm packages always nest under a single top-level directory named after
// the chart, so we accept any "*/Chart.yaml" entry.
func readChartMetadata(chartTgzPath string) (chartMetadata, error) {
	f, err := os.Open(chartTgzPath)
	if err != nil {
		return chartMetadata{}, fmt.Errorf("open chart: %w", err)
	}
	defer util.DeferClose(f)

	gz, err := gzip.NewReader(f)
	if err != nil {
		return chartMetadata{}, fmt.Errorf("gunzip chart: %w", err)
	}
	defer util.DeferClose(gz)

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return chartMetadata{}, fmt.Errorf("read chart tar: %w", err)
		}
		if path.Base(hdr.Name) != "Chart.yaml" {
			continue
		}
		// Make sure we only match the top-level Chart.yaml, not one belonging
		// to a vendored subchart under charts/.
		parts := strings.Split(hdr.Name, "/")
		if len(parts) != 2 {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return chartMetadata{}, fmt.Errorf("read Chart.yaml: %w", err)
		}
		var meta chartMetadata
		if err := yaml.Unmarshal(data, &meta); err != nil {
			return chartMetadata{}, fmt.Errorf("parse Chart.yaml: %w", err)
		}
		return meta, nil
	}
	return chartMetadata{}, fmt.Errorf("no top-level Chart.yaml found in %s", chartTgzPath)
}

func digestAndSize(b []byte) (string, int64) {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), int64(len(b))
}

func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer util.DeferClose(f)
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
