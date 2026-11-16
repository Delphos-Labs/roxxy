package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const conflictRepo, conflictTag = "library/alpha", "1.0"

// releasedTwice returns two builds published under the same repo:tag with
// different content: the one already in the registry and the one in the
// bundle being pushed.
func releasedTwice() (inRegistry, inBundle syntheticImage) {
	inRegistry = syntheticImage{
		registryID: "docker.io",
		repo:       conflictRepo,
		tag:        conflictTag,
		config:     makeBlob("config-first-release"),
		layers:     []syntheticBlob{makeBlob("layer-first-release")},
	}
	inRegistry.buildManifest()
	inBundle = syntheticImage{
		registryID: "docker.io",
		repo:       conflictRepo,
		tag:        conflictTag,
		config:     makeBlob("config-second-release"),
		layers:     []syntheticBlob{makeBlob("layer-second-release")},
	}
	inBundle.buildManifest()
	return inRegistry, inBundle
}

func sha256Digest(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func TestManifestEquivalenceDoesNotHideChangedIndex(t *testing.T) {
	fr := newFakeRegistry(t)
	rc, err := newRegistryClient(fr.URL(), 1)
	if err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":123}]}`)
	updated := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":123}]}`)
	fr.seedManifest("team/multiarch", "latest", old)
	equivalent, err := manifestsEquivalent(rc, "team/multiarch", "latest", updated)
	if err != nil {
		t.Fatal(err)
	}
	if equivalent {
		t.Fatal("changed index must not be skipped as content-equivalent")
	}
}

func TestPush_ConflictWithoutOverwriteKeepsExistingTag(t *testing.T) {
	old, bundled := releasedTwice()
	fr := newFakeRegistry(t)
	fr.seedManifest(conflictRepo, conflictTag, old.manifest)
	tarballPath := writeSyntheticTarball(t, []syntheticImage{bundled}, nil)

	coord, refs := buildCoordinator(t, fr, tarballPath, 2, pushOptions{})
	outcome, err := runWithDeadline(t, coord, refs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(outcome.conflicts) != 1 || len(outcome.overwrites) != 0 || len(outcome.failures) != 0 {
		t.Fatalf("outcome: conflicts=%d overwrites=%d failures=%d, want 1/0/0",
			len(outcome.conflicts), len(outcome.overwrites), len(outcome.failures))
	}
	if got := fr.manifestPushCount(conflictRepo); got != 0 {
		t.Errorf("manifest PUT %d times, want 0", got)
	}
	if got, want := fr.servedDigest(conflictRepo, conflictTag), sha256Digest(old.manifest); got != want {
		t.Errorf("served digest %s, want the original %s", got, want)
	}
	if code := pushAllExitCode(outcome, nil); code != 2 {
		t.Errorf("exit code %d, want 2 for refused content", code)
	}
}

func TestPush_ConflictWithOverwriteReplacesTag(t *testing.T) {
	old, bundled := releasedTwice()
	fr := newFakeRegistry(t)
	fr.seedManifest(conflictRepo, conflictTag, old.manifest)
	tarballPath := writeSyntheticTarball(t, []syntheticImage{bundled}, nil)

	coord, refs := buildCoordinator(t, fr, tarballPath, 2, pushOptions{overwrite: true})
	outcome, err := runWithDeadline(t, coord, refs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(outcome.overwrites) != 1 || len(outcome.conflicts) != 0 || len(outcome.failures) != 0 {
		t.Fatalf("outcome: overwrites=%d conflicts=%d failures=%d, want 1/0/0",
			len(outcome.overwrites), len(outcome.conflicts), len(outcome.failures))
	}
	if got, want := outcome.overwrites[0].conflictDgst, sha256Digest(old.manifest); got != want {
		t.Errorf("overwrite recorded previous digest %s, want %s", got, want)
	}
	if got, want := fr.servedDigest(conflictRepo, conflictTag), sha256Digest(bundled.manifest); got != want {
		t.Errorf("served digest %s, want the bundle's %s", got, want)
	}
	for _, b := range append([]syntheticBlob{bundled.config}, bundled.layers...) {
		if got := fr.totalUploads(b.digest); got != 1 {
			t.Errorf("blob %s uploaded %d times, want 1", b.digest, got)
		}
	}
	if code := pushAllExitCode(outcome, nil); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
}

func TestPush_RegistryRejectingOverwriteFailsWithRegistryError(t *testing.T) {
	old, bundled := releasedTwice()
	fr := newFakeRegistry(t)
	fr.seedManifest(conflictRepo, conflictTag, old.manifest)
	fr.rejectTagOverwrites()
	tarballPath := writeSyntheticTarball(t, []syntheticImage{bundled}, nil)

	coord, refs := buildCoordinator(t, fr, tarballPath, 2, pushOptions{overwrite: true})
	outcome, err := runWithDeadline(t, coord, refs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(outcome.failures) != 1 || len(outcome.overwrites) != 0 {
		t.Fatalf("outcome: failures=%d overwrites=%d, want 1/0", len(outcome.failures), len(outcome.overwrites))
	}
	reason := outcome.failures[0].failReason
	for _, want := range []string{"overwrite of " + sha256Digest(old.manifest) + " failed", "TAG_INVALID", "cannot be overwritten"} {
		if !strings.Contains(reason, want) {
			t.Errorf("failure reason %q does not contain %q", reason, want)
		}
	}
	if got, want := fr.servedDigest(conflictRepo, conflictTag), sha256Digest(old.manifest); got != want {
		t.Errorf("served digest %s, want the original %s", got, want)
	}
	if code := pushAllExitCode(outcome, nil); code != 2 {
		t.Errorf("exit code %d, want 2", code)
	}
}

// reencoded returns the same manifest with different bytes (indented JSON),
// so its digest differs while every descriptor is identical.
func reencoded(t *testing.T, manifest []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Indent(&out, manifest, "", "  "); err != nil {
		t.Fatalf("indent manifest: %v", err)
	}
	return out.Bytes()
}

func TestPush_MatchingTagIsSkipped(t *testing.T) {
	_, bundled := releasedTwice()
	for _, tc := range []struct {
		name   string
		stored []byte
	}{
		{"identical bytes", bundled.manifest},
		{"equivalent content", reencoded(t, bundled.manifest)},
	} {
		for _, overwrite := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/overwrite=%t", tc.name, overwrite), func(t *testing.T) {
				fr := newFakeRegistry(t)
				fr.seedManifest(conflictRepo, conflictTag, tc.stored)
				tarballPath := writeSyntheticTarball(t, []syntheticImage{bundled}, nil)

				coord, refs := buildCoordinator(t, fr, tarballPath, 2, pushOptions{overwrite: overwrite})
				outcome, err := runWithDeadline(t, coord, refs)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}

				if len(outcome.conflicts)+len(outcome.overwrites)+len(outcome.failures) != 0 {
					t.Errorf("outcome: conflicts=%d overwrites=%d failures=%d, want all 0",
						len(outcome.conflicts), len(outcome.overwrites), len(outcome.failures))
				}
				if got := fr.manifestPushCount(conflictRepo); got != 0 {
					t.Errorf("manifest PUT %d times, want 0", got)
				}
				for _, b := range append([]syntheticBlob{bundled.config}, bundled.layers...) {
					if got := fr.totalUploads(b.digest); got != 0 {
						t.Errorf("blob %s uploaded %d times, want 0", b.digest, got)
					}
				}
				if got, want := fr.servedDigest(conflictRepo, conflictTag), sha256Digest(tc.stored); got != want {
					t.Errorf("served digest %s, want the stored %s", got, want)
				}
			})
		}
	}
}

// TestHandlePush_OverwriteFlagReachesPush drives the real command entry point,
// so it fails if parsing drops --overwrite or a dispatch call site stops
// forwarding the parsed options. Only cases that end in success are listed:
// the others leave through os.Exit. If the single-ref path loses --overwrite,
// it refuses the conflict and calls os.Exit(1), which fails the package run
// without naming this subtest.
func TestHandlePush_OverwriteFlagReachesPush(t *testing.T) {
	old, bundled := releasedTwice()
	for _, tc := range []struct {
		name          string
		ref           string
		flags         []string
		wantOverwrite bool
	}{
		{"all with overwrite", "", []string{"--all", "--overwrite"}, true},
		{"single ref with overwrite", conflictRepo + ":" + conflictTag, []string{"--overwrite"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := newFakeRegistry(t)
			fr.seedManifest(conflictRepo, conflictTag, old.manifest)
			tarballPath := writeSyntheticTarball(t, []syntheticImage{bundled}, nil)

			args := []string{fr.URL()}
			if tc.ref != "" {
				args = append(args, tc.ref)
			}
			args = append(args, "--from-tarball", tarballPath)
			args = append(args, tc.flags...)
			HandlePush(Config{}, args)

			want := sha256Digest(old.manifest)
			if tc.wantOverwrite {
				want = sha256Digest(bundled.manifest)
			}
			if got := fr.servedDigest(conflictRepo, conflictTag); got != want {
				t.Errorf("served digest %s, want %s", got, want)
			}
		})
	}
}

func TestPushArgs_ChartRejectsOverwrite(t *testing.T) {
	if _, err := parsePushArgs([]string{"registry.example", "--chart", "mychart-0.1.0.tgz"}); err != nil {
		t.Fatalf("--chart without --overwrite: %v", err)
	}
	_, err := parsePushArgs([]string{"registry.example", "--chart", "mychart-0.1.0.tgz", "--overwrite"})
	if err == nil || !strings.Contains(err.Error(), "--chart always replaces the tag") {
		t.Fatalf("--chart --overwrite: err = %v, want the --chart always-replaces rejection", err)
	}
}

func TestPush_OverwriteSettlesEachRefIndependently(t *testing.T) {
	old, conflicting := releasedTwice()
	identical := syntheticImage{
		registryID: "docker.io",
		repo:       "team/identical",
		tag:        "2.0",
		config:     makeBlob("config-identical"),
		layers:     []syntheticBlob{makeBlob("layer-identical")},
	}
	identical.buildManifest()
	fresh := syntheticImage{
		registryID: "docker.io",
		repo:       "team/fresh",
		tag:        "3.0",
		config:     makeBlob("config-fresh"),
		layers:     []syntheticBlob{makeBlob("layer-fresh")},
	}
	fresh.buildManifest()

	fr := newFakeRegistry(t)
	fr.seedManifest(conflictRepo, conflictTag, old.manifest)
	fr.seedManifest(identical.repo, identical.tag, identical.manifest)
	tarballPath := writeSyntheticTarball(t, []syntheticImage{conflicting, identical, fresh}, nil)

	coord, refs := buildCoordinator(t, fr, tarballPath, 4, pushOptions{overwrite: true})
	if len(refs) != 3 {
		t.Fatalf("got %d refs, want 3", len(refs))
	}
	outcome, err := runWithDeadline(t, coord, refs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(outcome.conflicts) != 0 || len(outcome.failures) != 0 || len(outcome.overwrites) != 1 {
		t.Fatalf("outcome: overwrites=%d conflicts=%d failures=%d, want 1/0/0",
			len(outcome.overwrites), len(outcome.conflicts), len(outcome.failures))
	}
	if got := outcome.overwrites[0]; got.repoName != conflictRepo || got.conflictDgst != sha256Digest(old.manifest) {
		t.Errorf("overwrite recorded %s (was %s), want %s (was %s)",
			got.repoName, got.conflictDgst, conflictRepo, sha256Digest(old.manifest))
	}
	for _, tc := range []struct {
		img        syntheticImage
		wantPushes int
	}{
		{conflicting, 1},
		{identical, 0},
		{fresh, 1},
	} {
		if got, want := fr.servedDigest(tc.img.repo, tc.img.tag), sha256Digest(tc.img.manifest); got != want {
			t.Errorf("%s:%s served digest %s, want the bundle's %s", tc.img.repo, tc.img.tag, got, want)
		}
		if got := fr.manifestPushCount(tc.img.repo); got != tc.wantPushes {
			t.Errorf("%s manifest PUT %d times, want %d", tc.img.repo, got, tc.wantPushes)
		}
	}
	if code := pushAllExitCode(outcome, nil); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
}

// TestPush_UncomparableTagIsNotOverwritten: the registry reports a different
// digest for the tag, but its manifest can't be fetched or parsed. With
// --overwrite that must fail the ref, not replace a tag nobody compared;
// without it the tag is refused as a conflict, as before.
func TestPush_UncomparableTagIsNotOverwritten(t *testing.T) {
	old, bundled := releasedTwice()
	for _, fault := range []struct {
		name   string
		status int
		body   string
	}{
		{"GET 500", http.StatusInternalServerError, "upstream unavailable"},
		{"GET unparseable", http.StatusOK, "not a manifest"},
	} {
		for _, overwrite := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/overwrite=%t", fault.name, overwrite), func(t *testing.T) {
				fr := newFakeRegistry(t)
				fr.seedManifest(conflictRepo, conflictTag, old.manifest)
				fr.breakManifestGETs(fault.status, fault.body)
				tarballPath := writeSyntheticTarball(t, []syntheticImage{bundled}, nil)

				coord, refs := buildCoordinator(t, fr, tarballPath, 2, pushOptions{overwrite: overwrite})
				outcome, err := runWithDeadline(t, coord, refs)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}

				wantFailures, wantConflicts, wantExit := 0, 1, 2
				if overwrite {
					wantFailures, wantConflicts, wantExit = 1, 0, 2
				}
				if len(outcome.failures) != wantFailures || len(outcome.conflicts) != wantConflicts || len(outcome.overwrites) != 0 {
					t.Fatalf("outcome: failures=%d conflicts=%d overwrites=%d, want %d/%d/0",
						len(outcome.failures), len(outcome.conflicts), len(outcome.overwrites), wantFailures, wantConflicts)
				}
				if overwrite {
					want := "compare with existing " + sha256Digest(old.manifest) + " failed"
					if reason := outcome.failures[0].failReason; !strings.Contains(reason, want) {
						t.Errorf("failure reason %q does not contain %q", reason, want)
					}
				}
				if got := fr.manifestPushCount(conflictRepo); got != 0 {
					t.Errorf("manifest PUT %d times, want 0", got)
				}
				for _, b := range append([]syntheticBlob{bundled.config}, bundled.layers...) {
					if got := fr.totalUploads(b.digest); got != 0 {
						t.Errorf("blob %s uploaded %d times, want 0", b.digest, got)
					}
				}
				if got, want := fr.servedDigest(conflictRepo, conflictTag), sha256Digest(old.manifest); got != want {
					t.Errorf("served digest %s, want the original %s", got, want)
				}
				if code := pushAllExitCode(outcome, nil); code != wantExit {
					t.Errorf("exit code %d, want %d", code, wantExit)
				}
			})
		}
	}
}

func TestHandlePushProcess(t *testing.T) {
	if os.Getenv("ROXXY_TEST_HANDLE_PUSH") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			HandlePush(Config{}, os.Args[i+1:])
			return
		}
	}
	t.Fatal("missing push arguments")
}

func TestHandlePush_ConflictExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		all       bool
		overwrite bool
		immutable bool
		wantExit  int
	}{
		{"all refuses conflict", true, false, false, 2},
		{"single refuses conflict", false, false, false, 1},
		{"all overwrite succeeds", true, true, false, 0},
		{"single overwrite succeeds", false, true, false, 0},
		{"all registry rejects overwrite", true, true, true, 2},
		{"single registry rejects overwrite", false, true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, bundled := releasedTwice()
			fr := newFakeRegistry(t)
			fr.seedManifest(conflictRepo, conflictTag, old.manifest)
			if tc.immutable {
				fr.rejectTagOverwrites()
			}
			archive := writeSyntheticTarball(t, []syntheticImage{bundled}, nil)
			args := []string{"-test.run=^TestHandlePushProcess$", "--", fr.URL()}
			if !tc.all {
				args = append(args, conflictRepo+":"+conflictTag)
			}
			args = append(args, "--from-tarball", archive, "--workers", "2")
			if tc.all {
				args = append(args, "--all")
			}
			if tc.overwrite {
				args = append(args, "--overwrite")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], args...)
			cmd.Env = append(os.Environ(), "ROXXY_TEST_HANDLE_PUSH=1", "DOCKER_CONFIG="+t.TempDir())
			output, err := cmd.CombinedOutput()
			gotExit := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("run push: %v", err)
				}
				gotExit = exitErr.ExitCode()
			}
			if gotExit != tc.wantExit {
				t.Fatalf("exit %d, want %d: %s", gotExit, tc.wantExit, output)
			}
			want := old.manifest
			if tc.overwrite && !tc.immutable {
				want = bundled.manifest
			}
			if got := fr.servedDigest(conflictRepo, conflictTag); got != sha256Digest(want) {
				t.Errorf("served %s, want %s", got, sha256Digest(want))
			}
			if tc.immutable && !strings.Contains(string(output), "TAG_INVALID") {
				t.Errorf("registry rejection missing from output: %s", output)
			}
		})
	}
}
