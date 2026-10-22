package commands

import (
	"flag"
	"fmt"
	"os"

	"github.com/Delphos-Labs/roxxy/internal/cli"
	"github.com/Delphos-Labs/roxxy/internal/packages"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

// HandlePackage routes package subcommands
func HandlePackage(cfg Config, args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: package <command>")
		fmt.Println("Commands:")
		fmt.Println("  ls              List cached packages")
		fmt.Println("  parse [--type]  Parse index files for specified type (or all types)")
		os.Exit(1)
	}

	switch args[0] {
	case "ls":
		cliCfg := cli.Config{
			CacheDir:     cfg.CacheDir,
			StatsTracker: cfg.StatsTracker,
		}
		// package ls → cache ls --packages
		subargs := args[1:]
		cli.Run(cliCfg, append([]string{"--packages"}, subargs...))
	case "parse":
		// Use v2 parser
		HandlePackageParseV2(cfg, args[1:])
	default:
		fmt.Printf("Unknown command: %s\n", args[0])
		fmt.Println("Usage: package <command>")
		fmt.Println("Commands:")
		fmt.Println("  ls              List cached packages")
		fmt.Println("  parse [--type]  Parse index files for specified type (or all types)")
		os.Exit(1)
	}
}

// HandlePackageParse parses index files for the specified package type
func HandlePackageParse(cfg Config, args []string) {
	var pmType string
	fs := flag.NewFlagSet("package parse", flag.ExitOnError)
	fs.StringVar(&pmType, "type", "", "Package type to parse: apk, apt, npm, pypi, go (default: all types)")
	util.Logerr(fs.Parse(args))

	// Initialize indexDB if not already done
	indexDB := GetOrCreateIndexDB(cfg)

	typesToParse, err := GetPackageTypesToParse(pmType)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		fmt.Println("Valid types: apk, apt, npm, pypi, go")
		os.Exit(1)
	}

	fmt.Println("Parsing index files...")
	fmt.Println()

	fmt.Printf("Cache directory: %s\n", cfg.CacheDir)

	for _, pt := range typesToParse {
		fmt.Printf("Parsing %s index files...\n", string(pt))
		typeDir := PackageTypeDir(cfg.CacheDir, string(pt))
		fmt.Printf("  Type directory: %s\n", typeDir)

		// Check if directory exists
		if _, err := os.Stat(typeDir); os.IsNotExist(err) {
			fmt.Printf("  ⚠ Directory does not exist: %s\n", typeDir)
			continue
		}

		// Fix existing cached metadata files first (for Go packages)
		if pt == packages.TypeGo {
			fmt.Printf("  Fixing cached metadata files...\n")
			fixedCount, err := packages.FixCachedMetadata(cfg.CacheDir, pt)
			if err != nil {
				fmt.Printf("  ⚠ Warning: Error fixing metadata: %v\n", err)
			} else if fixedCount > 0 {
				fmt.Printf("  ✓ Fixed %d metadata file(s)\n", fixedCount)
			}
		}

		count, err := indexDB.ParseCachedIndexFiles(cfg.CacheDir, pt)
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			continue
		}
		if count > 0 {
			fmt.Printf("  ✓ Parsed %d index file(s)\n", count)
		} else {
			fmt.Printf("  ℹ No index files found for %s\n", string(pt))
		}
	}

	fmt.Println()
	fmt.Println("Done!")
}
