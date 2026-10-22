package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/Delphos-Labs/roxxy/internal/ui"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--help" || os.Args[1] == "-h") {
		fmt.Println("lsembed - List files in the embedded filesystem")
		fmt.Println("Usage: lsembed [path]")
		fmt.Println("  If path is provided, lists files in that directory")
		fmt.Println("  Otherwise, lists all embedded files")
		os.Exit(0)
	}

	var searchPath string
	if len(os.Args) > 1 {
		searchPath = os.Args[1]
	} else {
		searchPath = "."
	}

	// The embed is: ../../internal/ui/static
	// This creates a filesystem with "static" as the root (matching server.go)
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
