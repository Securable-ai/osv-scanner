package depsdev

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/osv-scalibr/enricher"
	"github.com/google/osv-scalibr/extractor"
	"github.com/google/osv-scalibr/extractor/filesystem/language/python/requirements"
	"github.com/google/osv-scalibr/inventory"
	"github.com/google/osv-scalibr/log"
	"github.com/google/osv-scalibr/plugin"
	"github.com/google/osv-scalibr/purl"
)

const (
	// PyPIDepsDevEnricherName is the unique name of this enricher.
	PyPIDepsDevEnricherName = "transitivedependency/requirements/depsdev"
)

// PyPIDepsDevEnricher performs dependency resolution for requirements.txt
// using the deps.dev REST API for pre-computed dependency graphs.
type PyPIDepsDevEnricher struct {
	client *PyPIDepsDevClient
}

// NewPyPIDepsDevEnricher creates a new enricher that uses deps.dev REST API.
func NewPyPIDepsDevEnricher(depsDevBaseURL string) (enricher.Enricher, error) {
	return &PyPIDepsDevEnricher{
		client: NewPyPIDepsDevClient(depsDevBaseURL),
	}, nil
}

// Name returns the name of the enricher.
func (e *PyPIDepsDevEnricher) Name() string {
	return PyPIDepsDevEnricherName
}

// Version returns the version of the enricher.
func (e *PyPIDepsDevEnricher) Version() int {
	return 0
}

// Requirements returns the requirements of the enricher.
func (e *PyPIDepsDevEnricher) Requirements() *plugin.Capabilities {
	return &plugin.Capabilities{
		Network: plugin.NetworkOnline,
	}
}

// RequiredPlugins returns the names of the plugins required by the enricher.
func (e *PyPIDepsDevEnricher) RequiredPlugins() []string {
	return []string{requirements.Name}
}

// Enrich enriches the inventory from requirements.txt with transitive dependencies
// fetched from the deps.dev REST API.
func (e *PyPIDepsDevEnricher) Enrich(ctx context.Context, input *enricher.ScanInput, inv *inventory.Inventory) error {
	// Group packages by location (requirements.txt path) and plugin name.
	// This is equivalent to internal.GroupPackagesFromPlugin but inlined to
	// avoid importing the internal package from osv-scalibr.
	pkgGroups := make(map[string]map[string]packageWithIndex)
	for i, pkg := range inv.Packages {
		if !slices.Contains(pkg.Plugins, requirements.Name) {
			continue
		}
		if len(pkg.Locations) == 0 {
			continue
		}
		path := pkg.Locations[0]
		if _, ok := pkgGroups[path]; !ok {
			pkgGroups[path] = make(map[string]packageWithIndex)
		}
		pkgGroups[path][pkg.Name] = packageWithIndex{pkg, i}
	}

	for path, pkgMap := range pkgGroups {
		pkgs, err := e.resolveGroup(ctx, path, pkgMap)
		if err != nil {
			log.Warnf("deps.dev resolution failed for %s: %v", path, err)
			continue
		}

		// Add resolved packages to inventory, equivalent to internal.Add
		for _, pkg := range pkgs {
			if indexPkg, ok := pkgMap[pkg.Name]; ok {
				// This dependency is in the manifest, update version and plugins.
				inv.Packages[indexPkg.index].Version = pkg.Version
				inv.Packages[indexPkg.index].Plugins = append(inv.Packages[indexPkg.index].Plugins, PyPIDepsDevEnricherName)
			} else {
				// Transitive dependency not in the manifest.
				inv.Packages = append(inv.Packages, pkg)
			}
		}
	}

	return nil
}

// packageWithIndex tracks a package along with its index in the inventory slice.
type packageWithIndex struct {
	pkg   *extractor.Package
	index int
}

// resolveGroup resolves transitive dependencies for all packages in a single requirements.txt.
func (e *PyPIDepsDevEnricher) resolveGroup(ctx context.Context, path string, pkgMap map[string]packageWithIndex) ([]*extractor.Package, error) {
	// Collect all transitive packages, deduplicating by name+version
	seen := make(map[string]bool)
	var result []*extractor.Package

	for _, indexPkg := range pkgMap {
		pkg := indexPkg.pkg
		if pkg.Version == "" {
			// Cannot look up packages without a pinned version
			continue
		}

		graph, err := e.client.GetDependencies(ctx, pkg.Name, pkg.Version)
		if err != nil {
			log.Warnf("deps.dev: failed to get dependencies for %s@%s: %v", pkg.Name, pkg.Version, err)
			continue
		}

		for _, node := range graph.Nodes {
			// Skip the SELF node
			if node.Relation == "SELF" {
				continue
			}

			// Normalize name to lowercase (PyPI is case-insensitive)
			name := strings.ToLower(node.VersionKey.Name)
			key := name + "@" + node.VersionKey.Version

			if seen[key] {
				continue
			}
			seen[key] = true

			result = append(result, &extractor.Package{
				Name:      name,
				Version:   node.VersionKey.Version,
				PURLType:  purl.TypePyPi,
				Locations: []string{path},
				Plugins:   []string{PyPIDepsDevEnricherName},
			})
		}
	}

	if len(result) == 0 && len(pkgMap) > 0 {
		return nil, fmt.Errorf("no dependencies resolved from deps.dev")
	}

	return result, nil
}
