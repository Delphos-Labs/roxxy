package cli

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Delphos-Labs/roxxy/internal/util"
)

// InstallConfig represents the install.json structure
type InstallConfig struct {
	Image   string   `json:"image"`   // Docker image (e.g., "roxxy:latest")
	Command []string `json:"command"` // Docker run command (e.g., ["docker", "run", "-it", "--rm"])
	Mounts  []string `json:"mounts"`  // Additional mounts (e.g., ["-v", "/host:/container"])
	Env     []string `json:"env"`     // Environment variables (e.g., ["-e", "KEY=value"])
	Args    []string `json:"args"`    // Default arguments to pass to container
}

// ReadInstallConfig reads install.json from a tarball
// Strategy:
// 1. Find and load the index (we need it anyway)
// 2. Try to read install.json right after the index (most likely location)
// 3. If not found, try index lookup (if install.json was included during export)
// 4. Fall back to sequential reading from the beginning
func ReadInstallConfig(tarballPath string) (*InstallConfig, error) {
	isGzip := strings.HasSuffix(tarballPath, ".gz") || strings.HasSuffix(tarballPath, ".tgz")

	// Strategy 1: Load the index first (we need it anyway)
	// Then try to read install.json right after the index
	if !isGzip {
		reader, err := OpenTarball(tarballPath)
		if err == nil {
			// Build index if not already built
			if !reader.indexBuilt {
				if err := reader.buildSectionIndex(); err == nil {
					// Index loaded successfully
					// Now try to find install.json right after the index
					// The index is at the end, so install.json should be the next entry

					// Find where the index ends by looking for it in fileOffsets
					// or by reading from near the end
					file, err := os.Open(tarballPath)
					if err == nil {
						defer util.DeferClose(file)

						fileInfo, err := file.Stat()
						if err == nil {
							fileSize := fileInfo.Size()

							// Try to find the index offset from the fileOffsets
							// The index file is named ".roxxy-index.json" or ".roxxy-index.json.gz"
							var indexHeaderOffset int64 = -1
							for key, offset := range reader.sectionIdx.fileOffsets {
								if strings.Contains(key, "roxxy-index") {
									indexHeaderOffset = offset
									break
								}
							}

							// If we found the index offset, read the index header to get its size,
							// then calculate where it ends and read install.json right after it
							if indexHeaderOffset >= 0 {
								// Seek to index header
								if _, err := file.Seek(indexHeaderOffset, io.SeekStart); err == nil {
									tarReader := tar.NewReader(file)
									header, err := tarReader.Next()
									if err == nil {
										path := stripCachePrefix(header.Name)
										if strings.Contains(path, "roxxy-index") && header.Typeflag == tar.TypeReg {
											// Skip the index data (we already loaded it into the index)
											if _, err := io.Copy(io.Discard, tarReader); err != nil {
												return nil, fmt.Errorf("failed to skip index data: %w", err)
											}

											// Now read from right after the index
											// The next entry should be install.json
											header, err := tarReader.Next()
											if err == nil {
												path := stripCachePrefix(header.Name)
												if path == "install.json" && header.Typeflag == tar.TypeReg {
													data := make([]byte, header.Size)
													if _, err := io.ReadFull(tarReader, data); err == nil {
														var config InstallConfig
														if err := json.Unmarshal(data, &config); err == nil {
															return &config, nil
														}
													}
												}
											}
										}
									}
								}
							}

							// If we didn't find it right after the index, try reading from near the end
							// (last 1MB should cover index + install.json)
							readSize := int64(1024 * 1024) // 1MB
							if readSize > fileSize {
								readSize = fileSize
							}
							startOffset := fileSize - readSize
							if startOffset < 0 {
								startOffset = 0
							}

							// Seek to near the end and read forward
							if _, err := file.Seek(startOffset, io.SeekStart); err == nil {
								tarReader := tar.NewReader(file)

								// Read entries forward looking for install.json
								for {
									header, err := tarReader.Next()
									if err == io.EOF {
										break
									}
									if err != nil {
										break
									}

									path := stripCachePrefix(header.Name)

									// Skip the index file itself
									if strings.Contains(path, "roxxy-index") {
										// Skip this entry
										if header.Typeflag == tar.TypeReg {
											util.LogerrT(io.Copy(io.Discard, tarReader))
										}
										continue
									}

									// Check if this is install.json
									if path == "install.json" && header.Typeflag == tar.TypeReg {
										data := make([]byte, header.Size)
										if _, err := io.ReadFull(tarReader, data); err == nil {
											var config InstallConfig
											if err := json.Unmarshal(data, &config); err == nil {
												return &config, nil
											}
										}
									}

									// Skip entry body if not install.json
									if header.Typeflag == tar.TypeReg {
										util.LogerrT(io.Copy(io.Discard, tarReader))
									}
								}
							}
						}
					}

					// Strategy 2: Try index lookup (if install.json was included during export)
					if offset, ok := reader.sectionIdx.fileOffsets["install.json"]; ok {
						// Found in index - seek directly to it
						file, tarReader, err := reader.createTarReader(offset)
						if err == nil {
							defer util.DeferClose(file)

							header, err := tarReader.Next()
							if err == nil {
								path := stripCachePrefix(header.Name)
								if path == "install.json" && header.Typeflag == tar.TypeReg {
									data := make([]byte, header.Size)
									if _, err := io.ReadFull(tarReader, data); err == nil {
										var config InstallConfig
										if err := json.Unmarshal(data, &config); err == nil {
											return &config, nil
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Strategy 3: Fall back to sequential reading from the beginning
	// This will find install.json even if it's in an unexpected location
	file, err := os.Open(tarballPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open tarball: %w", err)
	}
	defer util.DeferClose(file)

	var tarReader *tar.Reader
	if isGzip {
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer util.DeferClose(gzReader)
		tarReader = tar.NewReader(gzReader)
	} else {
		tarReader = tar.NewReader(file)
	}

	// Search for install.json from the beginning
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("install.json not found in tarball")
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tarball: %w", err)
		}

		// Strip cache/ prefix if present
		path := stripCachePrefix(header.Name)

		// Check if this is install.json
		if path == "install.json" || strings.HasSuffix(path, "/install.json") {
			if header.Typeflag != tar.TypeReg {
				continue
			}

			// Read install.json content
			data := make([]byte, header.Size)
			if _, err := io.ReadFull(tarReader, data); err != nil {
				return nil, fmt.Errorf("failed to read install.json: %w", err)
			}

			// Parse JSON
			var config InstallConfig
			if err := json.Unmarshal(data, &config); err != nil {
				return nil, fmt.Errorf("failed to parse install.json: %w", err)
			}

			return &config, nil
		}

		// Skip entry body if not install.json
		if header.Typeflag == tar.TypeReg {
			if _, err := io.Copy(io.Discard, tarReader); err != nil {
				return nil, fmt.Errorf("failed to skip entry: %w", err)
			}
		}
	}
}

// RunInstall executes the Docker container as specified in install.json
func RunInstall(tarballPath string, installConfig *InstallConfig, extraArgs []string) error {
	// Get absolute path to tarball for mounting
	absTarballPath, err := filepath.Abs(tarballPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Build docker run command
	cmd := exec.Command("docker")
	args := []string{"run"}

	// Add default command arguments from install.json
	// The command array can contain flags like "-it", "--rm", etc.
	if len(installConfig.Command) > 0 {
		for _, arg := range installConfig.Command {
			// Skip "docker" and "run" if present (they're already in our command)
			if arg == "docker" || arg == "run" {
				continue
			}
			args = append(args, arg)
		}
	}

	// Add mounts from install.json
	// Each mount should be a single string like "-v /host:/container"
	// or separate strings like ["-v", "/host:/container"]
	for _, mount := range installConfig.Mounts {
		// If mount contains spaces, split it into separate arguments
		if strings.Contains(mount, " ") {
			parts := strings.Fields(mount)
			args = append(args, parts...)
		} else {
			args = append(args, mount)
		}
	}

	// Mount the tarball
	// Default mount point: /cache.tar in container
	args = append(args, "-v", fmt.Sprintf("%s:/cache.tar:ro", absTarballPath))

	// Add environment variables from install.json
	// Each env should be like "-e KEY=value" or separate ["-e", "KEY=value"]
	for _, env := range installConfig.Env {
		// If env contains spaces, split it into separate arguments
		if strings.Contains(env, " ") {
			parts := strings.Fields(env)
			args = append(args, parts...)
		} else {
			args = append(args, env)
		}
	}

	// Add image
	args = append(args, installConfig.Image)

	// Add default args from install.json (these are arguments to the container command)
	args = append(args, installConfig.Args...)

	// Add extra arguments from command line (these override/extend install.json args)
	args = append(args, extraArgs...)

	cmd.Args = append(cmd.Args, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Printf("Running: docker %s\n", strings.Join(args, " "))
	return cmd.Run()
}
