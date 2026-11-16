package commands

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// HandleImport handles importing a roxxy cache tarball
func HandleImport(cfg Config, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: import <tarball>")
		fmt.Println("Example: import cache-export.tar.gz")
		os.Exit(1)
	}

	tarballPath := args[0]
	fmt.Printf("Importing cache from %s...\n", tarballPath)

	// Open and index the tarball
	reader, err := cli.OpenTarball(tarballPath)
	if err != nil {
		fmt.Printf("Error opening tarball: %v\n", err)
		os.Exit(1)
	}
	defer util.DeferClose(reader)

	// List what's in the tarball
	manifests, err := reader.ListManifests()
	if err != nil {
		fmt.Printf("Error listing manifests: %v\n", err)
		os.Exit(1)
	}
	blobs, err := reader.ListBlobs()
	if err != nil {
		fmt.Printf("Error listing blobs: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Found %d manifests and %d blobs in tarball\n", len(manifests), len(blobs))
	fmt.Println("\nManifests:")
	for _, path := range manifests {
		fmt.Printf("  %s\n", path)
	}
}

// HandlePush pushes a manifest tree from a tarball (the dominant case) or
// builds and pushes a helm chart manifest from a packaged .tgz on disk.
//
// Sources are mutually exclusive:
//
//	--from-tarball <path>   replay a cached ref from a roxxy tarball
//	--chart <chart.tgz>     build a helm OCI manifest from a packaged chart
//
// The artifact type (docker image, OCI image, helm chart) is implicit: the
// tarball already holds the right manifest, and the local-chart path always
// produces a helm-mediatype manifest.
func HandlePush(cfg Config, args []string) {
	req, err := parsePushArgs(args)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		if errors.Is(err, errPushUsage) {
			printPushUsage()
		}
		os.Exit(1)
	}
	switch {
	case req.pushAll:
		pushAllFromTarball(cfg, req.registry, req.tarballPath, req.platform, req.workers, req.opts)
	case req.tarballPath != "":
		pushFromTarball(cfg, req.registry, req.ref, req.tarballPath, req.platform, req.workers, req.opts)
	default:
		PushHelmChart(cfg, req.registry, req.chartPath, req.ref)
	}
}

// pushRequest is a validated `push` command line.
type pushRequest struct {
	registry    string
	ref         string
	tarballPath string
	chartPath   string
	platform    string
	workers     int
	pushAll     bool
	opts        pushOptions
}

var errPushUsage = errors.New("must specify --from-tarball or --chart (with --all to push every ref)")

// parsePushArgs parses `push <registry> [<repo:tag>] <flags>` and rejects
// flag combinations that have no meaning. An error wrapping errPushUsage
// means the caller should also print the usage text.
func parsePushArgs(args []string) (pushRequest, error) {
	if len(args) < 2 {
		return pushRequest{}, errPushUsage
	}
	req := pushRequest{registry: args[0], workers: DefaultPushWorkers}
	rest := args[1:]

	// Optional positional <repo:tag>. Anything that doesn't start with "--" is
	// treated as the ref. Required for --from-tarball without --all; optional
	// for --chart (defaults to chart name:version) and forbidden with --all.
	i := 0
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "--") {
		req.ref = rest[0]
		i = 1
	}
	for ; i < len(rest); i++ {
		switch rest[i] {
		case "--from-tarball":
			if i+1 >= len(rest) {
				return pushRequest{}, errors.New("--from-tarball requires a path")
			}
			req.tarballPath = rest[i+1]
			i++
		case "--chart":
			if i+1 >= len(rest) {
				return pushRequest{}, errors.New("--chart requires a path")
			}
			req.chartPath = rest[i+1]
			i++
		case "--platform":
			if i+1 >= len(rest) {
				return pushRequest{}, errors.New("--platform requires a value (e.g. linux/amd64)")
			}
			req.platform = rest[i+1]
			i++
		case "--workers":
			if i+1 >= len(rest) {
				return pushRequest{}, errors.New("--workers requires a value")
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil || n < 1 {
				return pushRequest{}, fmt.Errorf("--workers must be a positive integer (got %q)", rest[i+1])
			}
			req.workers = n
			i++
		case "--all":
			req.pushAll = true
		case "--coerce-docker-manifest":
			req.opts.coerceDocker = true
		case "--overwrite":
			req.opts.overwrite = true
		default:
			return pushRequest{}, fmt.Errorf("unknown flag: %s", rest[i])
		}
	}

	switch {
	case req.tarballPath != "" && req.chartPath != "":
		return pushRequest{}, errors.New("--from-tarball and --chart are mutually exclusive")
	case req.pushAll:
		if req.tarballPath == "" {
			return pushRequest{}, errors.New("--all requires --from-tarball")
		}
		if req.ref != "" {
			return pushRequest{}, errors.New("--all pushes every ref in the tarball; do not pass <repo:tag>")
		}
	case req.tarballPath != "":
		if req.ref == "" {
			return pushRequest{}, errors.New("<repo:tag> is required when pushing from a tarball (or pass --all)")
		}
	case req.chartPath != "":
		if req.platform != "" {
			return pushRequest{}, errors.New("--platform has no meaning for chart pushes")
		}
		if req.opts.coerceDocker {
			return pushRequest{}, errors.New("--coerce-docker-manifest is incompatible with --chart; helm charts must be pushed as OCI artifacts")
		}
		if req.opts.overwrite {
			return pushRequest{}, errors.New("--overwrite applies only to --from-tarball pushes; --chart always replaces the tag")
		}
	default:
		return pushRequest{}, errPushUsage
	}
	return req, nil
}

func printPushUsage() {
	fmt.Println("Usage:")
	fmt.Println("  push <registry> <repo:tag> --from-tarball <tarball> [--platform <os/arch>]")
	fmt.Println("       Replay a cached manifest tree from a roxxy tarball.")
	fmt.Println("       Works for docker images, OCI images, and helm charts.")
	fmt.Println("       --platform plucks one arch out of a multi-arch index.")
	fmt.Println()
	fmt.Println("  push <registry> --from-tarball <tarball> --all [--platform <os/arch>] [--workers N]")
	fmt.Println("       Push every manifest in the tarball, with byte-based progress.")
	fmt.Println("       --workers controls blob upload concurrency (default 4).")
	fmt.Println()
	fmt.Println("Optional flags (all push-from-tarball forms):")
	fmt.Println("  --coerce-docker-manifest")
	fmt.Println("       Force docker manifest v2 mediatype on push (for registries")
	fmt.Println("       that reject OCI image manifests). Refuses to coerce helm")
	fmt.Println("       charts — chart-as-docker is unusable.")
	fmt.Println("  --overwrite")
	fmt.Println("       Replace a tag that already points at different content.")
	fmt.Println("       Without it, such tags are left untouched and reported as")
	fmt.Println("       conflicts (--all exits 2; the single-ref form exits 1).")
	fmt.Println("       If the registry refuses the replacement (e.g. an immutable")
	fmt.Println("       ECR repository), its error is reported and roar exits non-zero.")
	fmt.Println()
	fmt.Println("  push <registry> [<repo:tag>] --chart <chart.tgz>")
	fmt.Println("       Package and push a local helm chart to an OCI registry.")
	fmt.Println("       <repo:tag> defaults to the chart's name:version from Chart.yaml.")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  push 123.dkr.ecr.us-east-1.amazonaws.com my-team/api:v1 --from-tarball cache.tar")
	fmt.Println("  push 123.dkr.ecr.us-east-1.amazonaws.com --from-tarball cache.tar --all --platform linux/amd64")
	fmt.Println("  push 123.dkr.ecr.us-east-1.amazonaws.com --from-tarball cache.tar --all --overwrite")
	fmt.Println("  push 123.dkr.ecr.us-east-1.amazonaws.com --chart mychart-0.1.0.tgz")
}

// HandleDeploy handles deployment to various targets (Helm, k8s, etc.)
func HandleDeploy(cfg Config, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: deploy <target> [options]")
		fmt.Println("Targets:")
		fmt.Println("  helm <chart> [values]  Deploy via Helm to Kubernetes")
		fmt.Println("Example: deploy helm my-chart --values values.yaml --from-tarball cache.tar.gz")
		os.Exit(1)
	}

	target := args[0]
	_ = args[1:] // Will be used when implementing Helm deployment

	switch target {
	case "helm":
		fmt.Println("Helm deployment not yet implemented")
		os.Exit(1)
	default:
		fmt.Printf("Unknown deploy target: %s\n", target)
		os.Exit(1)
	}
}

// pushDockerImage is implemented in docker_push.go
