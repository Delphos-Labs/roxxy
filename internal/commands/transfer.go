package commands

import (
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
	if len(args) < 2 {
		printPushUsage()
		os.Exit(1)
	}

	registry := args[0]
	rest := args[1:]

	// Optional positional <repo:tag>. Anything that doesn't start with "--" is
	// treated as the ref. Required for --from-tarball without --all; optional
	// for --chart (defaults to chart name:version) and forbidden with --all.
	var ref string
	var tarballPath, chartPath, platform string
	var pushAll bool
	var coerceDocker bool
	workers := DefaultPushWorkers

	i := 0
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "--") {
		ref = rest[0]
		i = 1
	}
	for ; i < len(rest); i++ {
		switch rest[i] {
		case "--from-tarball":
			if i+1 >= len(rest) {
				fmt.Println("Error: --from-tarball requires a path")
				os.Exit(1)
			}
			tarballPath = rest[i+1]
			i++
		case "--chart":
			if i+1 >= len(rest) {
				fmt.Println("Error: --chart requires a path")
				os.Exit(1)
			}
			chartPath = rest[i+1]
			i++
		case "--platform":
			if i+1 >= len(rest) {
				fmt.Println("Error: --platform requires a value (e.g. linux/amd64)")
				os.Exit(1)
			}
			platform = rest[i+1]
			i++
		case "--workers":
			if i+1 >= len(rest) {
				fmt.Println("Error: --workers requires a value")
				os.Exit(1)
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil || n < 1 {
				fmt.Printf("Error: --workers must be a positive integer (got %q)\n", rest[i+1])
				os.Exit(1)
			}
			workers = n
			i++
		case "--all":
			pushAll = true
		case "--coerce-docker-manifest":
			coerceDocker = true
		default:
			fmt.Printf("Error: unknown flag: %s\n", rest[i])
			os.Exit(1)
		}
	}

	switch {
	case tarballPath != "" && chartPath != "":
		fmt.Println("Error: --from-tarball and --chart are mutually exclusive")
		os.Exit(1)
	case pushAll:
		if tarballPath == "" {
			fmt.Println("Error: --all requires --from-tarball")
			os.Exit(1)
		}
		if ref != "" {
			fmt.Println("Error: --all pushes every ref in the tarball; do not pass <repo:tag>")
			os.Exit(1)
		}
		if chartPath != "" {
			fmt.Println("Error: --all and --chart are mutually exclusive")
			os.Exit(1)
		}
		pushAllFromTarball(cfg, registry, tarballPath, platform, workers, coerceDocker)
	case tarballPath != "":
		if ref == "" {
			fmt.Println("Error: <repo:tag> is required when pushing from a tarball (or pass --all)")
			os.Exit(1)
		}
		pushFromTarball(cfg, registry, ref, tarballPath, platform, workers, coerceDocker)
	case chartPath != "":
		if platform != "" {
			fmt.Println("Error: --platform has no meaning for chart pushes")
			os.Exit(1)
		}
		if coerceDocker {
			fmt.Println("Error: --coerce-docker-manifest is incompatible with --chart; helm charts must be pushed as OCI artifacts")
			os.Exit(1)
		}
		PushHelmChart(cfg, registry, chartPath, ref)
	default:
		fmt.Println("Error: must specify --from-tarball or --chart (with --all to push every ref)")
		printPushUsage()
		os.Exit(1)
	}
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
	fmt.Println()
	fmt.Println("  push <registry> [<repo:tag>] --chart <chart.tgz>")
	fmt.Println("       Package and push a local helm chart to an OCI registry.")
	fmt.Println("       <repo:tag> defaults to the chart's name:version from Chart.yaml.")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  push 123.dkr.ecr.us-east-1.amazonaws.com my-team/api:v1 --from-tarball cache.tar")
	fmt.Println("  push 123.dkr.ecr.us-east-1.amazonaws.com --from-tarball cache.tar --all --platform linux/amd64")
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
