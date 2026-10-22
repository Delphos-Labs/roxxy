package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/commands"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// BuildTime is set at build time using -ldflags
var BuildTime = "unknown"

func main() {
	if len(os.Args) < 2 {
		showUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// Minimal config - only what's needed for tarball operations
	// Most fields are nil/empty since we don't need cache, stats, or index for tarball-only operations
	cfg := commands.Config{
		CacheDir:     "",  // Not needed for tarball operations
		CertsDir:     "",  // Not needed for tarball operations
		LogDir:       "",  // Not needed for tarball operations
		ConfigFile:   "",  // Not needed for tarball operations
		StatsTracker: nil, // Not needed for tarball operations
		IndexDB:      nil, // Not needed for tarball operations
		BuildTime:    BuildTime,
		GlobalConfig: nil, // Not needed for tarball operations
	}

	switch cmd {
	case "ls", "list":
		handleList(cfg, args)
	case "import":
		commands.HandleImport(cfg, args)
	case "push":
		commands.HandlePush(cfg, args)
	case "export":
		handleExport(cfg, args)
	case "install":
		handleInstall(cfg, args)
	case "info":
		handleInfo(cfg, args)
	case "help", "--help", "-h":
		showUsage()
	default:
		fmt.Printf("Unknown command: %s\n\n", cmd)
		showUsage()
		os.Exit(1)
	}
}

func showUsage() {
	fmt.Println("roar - Roxxy Archiver")
	fmt.Println("A CLI tool for interacting with roxxy cache tarballs")
	fmt.Println()
	fmt.Println("Usage: roar <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  ls, list [--from-tarball <tarball>] [--docker] [--image <image>] [--platform <os/arch>]")
	fmt.Println("           List contents of a tarball or show image layers")
	fmt.Println("  import <tarball>")
	fmt.Println("           Import a roxxy cache tarball (list contents)")
	fmt.Println("  push <registry> <repo:tag> --from-tarball <tarball> [--platform <os/arch>]")
	fmt.Println("           Replay a cached manifest tree (image or chart) from a tarball.")
	fmt.Println("           --platform plucks one arch from a multi-arch index.")
	fmt.Println("  push <registry> --from-tarball <tarball> --all [--platform <os/arch>]")
	fmt.Println("           Push every manifest in the tarball with byte-based progress.")
	fmt.Println("  push <registry> [<repo:tag>] --chart <chart.tgz>")
	fmt.Println("           Build and push a helm OCI manifest from a packaged chart.")
	fmt.Println("  export docker <image> [output.tar] [--from-tarball <tarball>]")
	fmt.Println("           Export Docker image as Docker tarball (compatible with docker load)")
	fmt.Println("  export oci <image> [output.tar] [--from-tarball <tarball>] [--platform <platform>]")
	fmt.Println("           Export Docker image(s) as OCI tarball (use - for stdout)")
	fmt.Println("           Multiple images can be specified: export oci <image1> <image2> ...")
	fmt.Println("  install <tarball> [extra args...]")
	fmt.Println("           Install from tarball using install.json (runs Docker container with tarball mounted)")
	fmt.Println("  info <tarball>")
	fmt.Println("           Show information about a tarball (size, contents, etc.)")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  roar ls --from-tarball cache.tar --docker")
	fmt.Println("  roar ls --from-tarball cache.tar --docker --image ghcr.io/org/image:tag")
	fmt.Println("  roar import cache.tar.gz")
	fmt.Println("  roar push registry.example.com library/golang:1.25 --from-tarball cache.tar")
	fmt.Println("  roar push 123.dkr.ecr.us-east-1.amazonaws.com my-team/api:v1 --from-tarball cache.tar --platform linux/amd64")
	fmt.Println("  roar push 123.dkr.ecr.us-east-1.amazonaws.com --from-tarball cache.tar --all")
	fmt.Println("  roar push 123.dkr.ecr.us-east-1.amazonaws.com --chart mychart-0.1.0.tgz")
	fmt.Println("  roar export docker library/golang:1.25-alpine image.tar --from-tarball cache.tar")
	fmt.Println("  roar install cache.tar --extra-arg")
	fmt.Println()
	fmt.Println("install.json format:")
	fmt.Println("  {")
	fmt.Println("    \"image\": \"roxxy:latest\",")
	fmt.Println("    \"command\": [\"docker\", \"run\", \"-it\", \"--rm\"],")
	fmt.Println("    \"mounts\": [\"-v\", \"/host:/container\"],")
	fmt.Println("    \"env\": [\"-e\", \"KEY=value\"],")
	fmt.Println("    \"args\": [\"default\", \"args\"]")
	fmt.Println("  }")
}

func handleList(cfg commands.Config, args []string) {
	// Parse flags for ls command
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	fromTarball := fs.String("from-tarball", "", "Tarball file to read from")
	dockerMode := fs.Bool("docker", false, "List Docker images")
	imageName := fs.String("image", "", "Show layers for specific Docker image")
	platform := fs.String("platform", "", "Platform for multi-platform images (e.g., linux/amd64)")
	debug := fs.Bool("debug", false, "Enable debug logging")
	util.Logerr(fs.Parse(args))

	if *fromTarball == "" {
		fmt.Println("Error: --from-tarball is required for roar")
		fmt.Println("Usage: roar ls --from-tarball <tarball> [--docker] [--image <image>]")
		os.Exit(1)
	}

	// Use cli.Run with minimal config
	cliCfg := cli.Config{
		CacheDir:     "",  // Not used when reading from tarball
		StatsTracker: nil, // Not needed for tarball operations
	}

	// Build args for cli.Run
	cliArgs := []string{"--from-tarball", *fromTarball}
	if *dockerMode {
		cliArgs = append(cliArgs, "--docker")
	}
	if *imageName != "" {
		cliArgs = append(cliArgs, "--image", *imageName)
	}
	if *platform != "" {
		cliArgs = append(cliArgs, "--platform", *platform)
	}
	if *debug {
		cliArgs = append(cliArgs, "--debug")
	}

	cli.Run(cliCfg, cliArgs)
}

func handleExport(cfg commands.Config, args []string) {
	if len(args) == 0 {
		fmt.Println("Error: export target required")
		fmt.Println("Usage: roar export docker <image> [output.tar] [--from-tarball <tarball>]")
		os.Exit(1)
	}

	target := args[0]
	subargs := args[1:]

	switch target {
	case "docker":
		if len(subargs) == 0 {
			fmt.Println("Error: image name required")
			fmt.Println("Usage: roar export docker <repo:tag> [output.tar] [--from-tarball <tarball>]")
			os.Exit(1)
		}
		imageSpec := subargs[0]
		outputFile := ""
		tarballPath := ""

		// Parse arguments
		for i := 1; i < len(subargs); i++ {
			if subargs[i] == "--from-tarball" && i+1 < len(subargs) {
				tarballPath = subargs[i+1]
				i++ // Skip the tarball path
			} else if outputFile == "" {
				outputFile = subargs[i]
			}
		}

		// For roar, tarball is required
		if tarballPath == "" {
			fmt.Println("Error: --from-tarball is required for roar")
			fmt.Println("Usage: roar export docker <repo:tag> [output.tar] --from-tarball <tarball>")
			os.Exit(1)
		}

		commands.ExportDockerImageToDockerTarball(cfg, imageSpec, outputFile, tarballPath)
	case "oci":
		if len(subargs) == 0 {
			fmt.Println("Error: at least one image name required")
			fmt.Println("Usage: roar export oci <repo:tag> [<repo:tag> ...] [output.tar] [--from-tarball <tarball>] [--platform <platform>]")
			os.Exit(1)
		}

		var imageSpecs []string
		outputFile := ""
		tarballPath := ""
		platform := ""

		// Parse arguments - collect image specs and flags
		for i := 0; i < len(subargs); i++ {
			arg := subargs[i]

			// Handle flags first
			if arg == "--from-tarball" {
				if i+1 < len(subargs) {
					tarballPath = subargs[i+1]
					i++ // Skip the tarball path
				} else {
					fmt.Fprintf(os.Stderr, "Error: --from-tarball requires a value\n")
					os.Exit(1)
				}
				continue
			}

			if arg == "--platform" {
				if i+1 < len(subargs) {
					platform = subargs[i+1]
					i++ // Skip the platform value
				} else {
					fmt.Fprintf(os.Stderr, "Error: --platform requires a value\n")
					os.Exit(1)
				}
				continue
			}

			// Skip any other flags (starting with --)
			if strings.HasPrefix(arg, "--") {
				fmt.Fprintf(os.Stderr, "Warning: Unknown flag: %s\n", arg)
				continue
			}

			// Check if this looks like an image spec (repo:tag or repo@digest)
			if strings.Contains(arg, ":") || strings.Contains(arg, "@") {
				imageSpecs = append(imageSpecs, arg)
				continue
			}

			// First non-flag, non-image argument is the output file
			if outputFile == "" {
				outputFile = arg
			} else {
				fmt.Fprintf(os.Stderr, "Warning: Unexpected argument: %s (ignored)\n", arg)
			}
		}

		if len(imageSpecs) == 0 {
			fmt.Println("Error: at least one image name required")
			fmt.Println("Usage: roar export oci <repo:tag> [<repo:tag> ...] [output.tar] --from-tarball <tarball> [--platform <platform>]")
			os.Exit(1)
		}

		// For roar, tarball is required
		if tarballPath == "" {
			fmt.Println("Error: --from-tarball is required for roar")
			fmt.Println("Usage: roar export oci <repo:tag> [<repo:tag> ...] [output.tar] --from-tarball <tarball> [--platform <platform>]")
			os.Exit(1)
		}

		if len(imageSpecs) == 1 {
			// Single image - use existing function
			commands.ExportDockerImageToOCITarball(cfg, imageSpecs[0], outputFile, tarballPath, platform)
		} else {
			// Multiple images - use new function
			commands.ExportMultipleDockerImagesToOCITarball(cfg, imageSpecs, outputFile, tarballPath, platform)
		}
	default:
		fmt.Printf("Unknown export target: %s\n", target)
		fmt.Println("Supported targets: docker, oci")
		os.Exit(1)
	}
}

func handleInfo(cfg commands.Config, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: roar info <tarball>")
		os.Exit(1)
	}

	tarballPath := args[0]

	// Check if file exists
	info, err := os.Stat(tarballPath)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// Open tarball to get contents
	reader, err := cli.OpenTarball(tarballPath)
	if err != nil {
		fmt.Printf("Error opening tarball: %v\n", err)
		os.Exit(1)
	}
	defer util.DeferClose(reader)

	// Get manifest and blob counts
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

	// Display info
	fmt.Printf("Tarball: %s\n", filepath.Base(tarballPath))
	fmt.Printf("Size: %s\n", formatBytes(info.Size()))
	fmt.Printf("Manifests: %d\n", len(manifests))
	fmt.Printf("Blobs: %d\n", len(blobs))
	fmt.Printf("Modified: %s\n", info.ModTime().Format("2006-01-02 15:04:05"))
}

func handleInstall(cfg commands.Config, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: roar install <tarball> [extra args...]")
		fmt.Println("")
		fmt.Println("Install from tarball using install.json (runs Docker container with tarball mounted)")
		fmt.Println("")
		fmt.Println("Example:")
		fmt.Println("  roar install cache.tar")
		fmt.Println("  roar install cache.tar --extra-arg --another-arg")
		os.Exit(1)
	}

	tarballPath := args[0]
	extraArgs := args[1:]

	// Check if file exists
	if _, err := os.Stat(tarballPath); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// Read install.json from tarball
	installConfig, err := cli.ReadInstallConfig(tarballPath)
	if err != nil {
		fmt.Printf("Error reading install.json: %v\n", err)
		fmt.Println("")
		fmt.Println("The tarball must contain an install.json file with the following format:")
		fmt.Println("  {")
		fmt.Println("    \"image\": \"roxxy:latest\",")
		fmt.Println("    \"command\": [\"-it\", \"--rm\"],")
		fmt.Println("    \"mounts\": [\"-v\", \"/host:/container\"],")
		fmt.Println("    \"env\": [\"-e\", \"KEY=value\"],")
		fmt.Println("    \"args\": [\"server\"]")
		fmt.Println("  }")
		os.Exit(1)
	}

	// Validate install config
	if installConfig.Image == "" {
		fmt.Println("Error: install.json must specify an 'image' field")
		os.Exit(1)
	}

	// Run the install
	if err := cli.RunInstall(tarballPath, installConfig, extraArgs); err != nil {
		fmt.Printf("Error running install: %v\n", err)
		os.Exit(1)
	}
}

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
