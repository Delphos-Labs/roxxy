package commands

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// parseImageListFile parses a file containing image specifications and platforms.
// Format: one image per line, with optional platforms:
//
//	alpine:latest linux/amd64
//	golang:1.25-alpine linux/arm64 linux/amd64
//	library/golang:1.25-alpine
//
// Returns a slice of ImageSpec structs.
func parseImageListFile(filePath string) ([]ImageSpec, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer util.DeferClose(file)

	var specs []ImageSpec
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split line into parts
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}

		// First part is the image name
		imageSpec := ImageSpec{
			Image:     parts[0],
			Platforms: []string{},
		}

		// Remaining parts are platforms
		if len(parts) > 1 {
			imageSpec.Platforms = parts[1:]
		}

		specs = append(specs, imageSpec)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading file at line %d: %w", lineNum, err)
	}

	if len(specs) == 0 {
		return nil, fmt.Errorf("no valid image specifications found in file")
	}

	return specs, nil
}

// ImageSpec represents an image specification with optional platforms
type ImageSpec struct {
	Image     string
	Platforms []string
}
