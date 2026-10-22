package commands

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/Delphos-Labs/roxxy/internal/ui"
)

// HandleEmbed routes embed subcommands
func HandleEmbed(cfg Config, args []string) {
	if len(args) == 0 {
		args = []string{"."}
	}

	// Import and run lsembed logic
	runLsembed(args)
}

// runLsembed runs the lsembed logic
func runLsembed(args []string) {
	searchPath := "."
	if len(args) > 0 {
		searchPath = args[0]
	}

	embedRoot := "static"
	if searchPath != "." {
		embedRoot = filepath.Join(embedRoot, searchPath)
	}

	// Get the embedded filesystem from ui package
	staticFiles := ui.GetStaticFiles()

	var files []string
	err := fs.WalkDir(staticFiles, embedRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Get relative path from "static" root
		relPath, err := filepath.Rel("static", path)
		if err != nil {
			relPath = path
		}

		files = append(files, relPath)
		return nil
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error walking filesystem: %v\n", err)
		os.Exit(1)
	}

	// Sort files for consistent output
	sort.Strings(files)

	// Print files with details
	for _, file := range files {
		fullPath := filepath.Join("static", file)
		info, err := fs.Stat(staticFiles, fullPath)
		if err != nil {
			fmt.Printf("  %s (error: %v)\n", file, err)
			continue
		}

		if info.IsDir() {
			fmt.Printf("  %s/\n", file)
		} else {
			fmt.Printf("  %s (%d bytes)\n", file, info.Size())
		}
	}

	if len(files) == 0 {
		fmt.Println("No files found in embedded filesystem")
		os.Exit(1)
	}
}
