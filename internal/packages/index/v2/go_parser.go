package v2

// GoParserV2 parses Go module index files in v2 format
// Note: We don't parse .info files - they only contain minimal metadata (version, time)
// All useful data (version, dependencies, license, description) comes from .zip files
// Go packages are discovered by scanning cached .zip files directly via AddCachedPackagesToIndexV2
type GoParserV2 struct{}

// CanParse returns false - we don't parse .info files anymore
// Go packages are discovered by scanning cached .zip files via AddCachedPackagesToIndexV2
func (p *GoParserV2) CanParse(indexPath string) bool {
	return false // Don't parse .info files - they don't add value
}

// Parse extracts package metadata from Go module index files
// Note: This is a no-op now - we don't parse .info files
// Go packages are discovered by scanning cached .zip files via AddCachedPackagesToIndexV2
func (p *GoParserV2) Parse(data []byte, indexURL string, contentType string, cachePath string) ([]IndexPackageV2, error) {
	// Don't parse .info files - they don't add value
	// All Go packages are discovered from cached .zip files
	return nil, nil
}
