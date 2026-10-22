package index

import (
	"github.com/Delphos-Labs/roxxy/internal/packages"
)

// IndexDBInterfaceAdapter adapts IndexDB to packages.IndexDBInterface
// This is defined in the index package to avoid import cycles
type IndexDBInterfaceAdapter struct {
	db *IndexDB
}

// NewIndexDBInterfaceAdapter creates an adapter that implements packages.IndexDBInterface
func NewIndexDBInterfaceAdapter(db *IndexDB) packages.IndexDBInterface {
	return &IndexDBInterfaceAdapter{db: db}
}

// ParseIndexFile implements packages.IndexDBInterface
func (a *IndexDBInterfaceAdapter) ParseIndexFile(cachePath string, indexURL string, pmType packages.Type, contentType string) error {
	return a.db.ParseIndexFile(cachePath, indexURL, pmType, contentType)
}

// GetPackages implements packages.IndexDBInterface
func (a *IndexDBInterfaceAdapter) GetPackages(pmType packages.Type) ([]interface{}, error) {
	pkgs, err := a.db.GetPackages(pmType)
	if err != nil {
		return nil, err
	}
	result := make([]interface{}, len(pkgs))
	for i := range pkgs {
		result[i] = &pkgs[i]
	}
	return result, nil
}

// UpdatePackageMetadata implements packages.IndexDBInterface
func (a *IndexDBInterfaceAdapter) UpdatePackageMetadata(pmType packages.Type, name, version string, updates func(interface{})) error {
	return a.db.UpdatePackageMetadata(pmType, name, version, updates)
}
