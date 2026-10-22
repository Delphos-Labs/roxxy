package commands

import (
	"fmt"
	"os"
	"path/filepath"
)

// HandleHardlink creates hardlinks to the binary for convenience commands
func HandleHardlink(cfg Config) {
	selfPath := os.Args[0]

	// Get the directory where the binary is located
	binaryDir := "/usr/bin"

	// Hardlinks to create
	links := []string{"docker", "raw", "package", "embed"}

	fmt.Println("Creating hardlinks for convenience commands...")

	for _, linkName := range links {
		linkPath := filepath.Join(binaryDir, linkName)

		// Remove existing file/link if it exists
		if err := os.Remove(linkPath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("  Warning: Failed to remove existing %s: %v\n", linkName, err)
			continue
		}

		// Create hardlink
		if err := os.Link(selfPath, linkPath); err != nil {
			fmt.Printf("  Error: Failed to create hardlink %s: %v\n", linkName, err)
			os.Exit(1)
		}

		fmt.Printf("  Created: %s\n", linkPath)
	}

	fmt.Println("Hardlinks created successfully!")
}
