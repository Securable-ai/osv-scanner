package depsdev

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/osv-scalibr/enricher"
	"github.com/google/osv-scalibr/extractor"
	"github.com/google/osv-scalibr/extractor/filesystem/language/java/javalockfile"
	"github.com/google/osv-scalibr/inventory"
	"github.com/google/osv-scalibr/log"
	"github.com/google/osv-scalibr/plugin"
	"github.com/google/osv-scalibr/purl"
)

const (
	// MavenDepsDevEnricherName is the unique name of this enricher.
	MavenDepsDevEnricherName = "transitivedependency/maven/depsdev"

	// pomxmlExtractorName matches the offline pomxml extractor plugin name.
	pomxmlExtractorName = "java/pomxml"
	// pomxmlEnhanceableName matches the enhanceable pomxml extractor plugin name.
	pomxmlEnhanceableName = "java/pomxmlenhanceable"
	// pomxmlNetName matches the online pomxmlnet extractor plugin name.
	pomxmlNetName = "java/pomxmlnet"
)

// MavenDepsDevEnricher performs dependency resolution for pom.xml
// using the deps.dev REST API for pre-computed dependency graphs.
type MavenDepsDevEnricher struct {
	client *DepsDevRESTClient
}

// NewMavenDepsDevEnricher creates a new enricher that uses deps.dev REST API for Maven.
func NewMavenDepsDevEnricher(depsDevBaseURL string) (enricher.Enricher, error) {
	return &MavenDepsDevEnricher{
		client: NewMavenDepsDevClient(depsDevBaseURL),
	}, nil
}

// Name returns the name of the enricher.
func (e *MavenDepsDevEnricher) Name() string {
	return MavenDepsDevEnricherName
}

// Version returns the version of the enricher.
func (e *MavenDepsDevEnricher) Version() int {
	return 0
}

// Requirements returns the requirements of the enricher.
func (e *MavenDepsDevEnricher) Requirements() *plugin.Capabilities {
	return &plugin.Capabilities{
		Network: plugin.NetworkOnline,
	}
}

// RequiredPlugins returns the names of the plugins required by the enricher.
func (e *MavenDepsDevEnricher) RequiredPlugins() []string {
	return []string{pomxmlEnhanceableName}
}

// isMavenPlugin checks if a plugin name is a Maven pom.xml extractor.
func isMavenPlugin(name string) bool {
	return name == pomxmlExtractorName || name == pomxmlEnhanceableName || name == pomxmlNetName
}

// Enrich enriches the inventory from pom.xml with transitive dependencies
// fetched from the deps.dev REST API.
func (e *MavenDepsDevEnricher) Enrich(ctx context.Context, input *enricher.ScanInput, inv *inventory.Inventory) error {
	// Group packages by location (pom.xml path) from Maven extractors.
	pkgGroups := make(map[string]map[string]packageWithIndex)
	for i, pkg := range inv.Packages {
		isMaven := false
		for _, p := range pkg.Plugins {
			if isMavenPlugin(p) {
				isMaven = true
				break
			}
		}
		if !isMaven {
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
			log.Warnf("deps.dev Maven resolution failed for %s: %v", path, err)
			continue
		}

		// Add resolved packages to inventory.
		for _, pkg := range pkgs {
			if indexPkg, ok := pkgMap[pkg.Name]; ok {
				// This dependency is in the manifest, update version and plugins.
				inv.Packages[indexPkg.index].Version = pkg.Version
				if !slices.Contains(inv.Packages[indexPkg.index].Plugins, MavenDepsDevEnricherName) {
					inv.Packages[indexPkg.index].Plugins = append(inv.Packages[indexPkg.index].Plugins, MavenDepsDevEnricherName)
				}
			} else {
				// Transitive dependency not in the manifest.
				inv.Packages = append(inv.Packages, pkg)
			}
		}
	}

	return nil
}

// resolveGroup resolves transitive dependencies for all packages in a single pom.xml.
func (e *MavenDepsDevEnricher) resolveGroup(ctx context.Context, path string, pkgMap map[string]packageWithIndex) ([]*extractor.Package, error) {
	// Collect all transitive packages, deduplicating by name+version
	seen := make(map[string]bool)
	var result []*extractor.Package

	for _, indexPkg := range pkgMap {
		pkg := indexPkg.pkg
		if pkg.Version == "" {
			continue
		}

		// Maven name format is "groupId:artifactId"
		graph, err := e.client.GetDependencies(ctx, pkg.Name, pkg.Version)
		if err != nil {
			log.Warnf("deps.dev: failed to get Maven dependencies for %s@%s: %v", pkg.Name, pkg.Version, err)
			continue
		}

		for _, node := range graph.Nodes {
			// Skip the SELF node
			if node.Relation == "SELF" {
				continue
			}

			name := node.VersionKey.Name // Maven names are already in groupId:artifactId format
			key := name + "@" + node.VersionKey.Version

			if seen[key] {
				continue
			}
			seen[key] = true

			// Split groupId:artifactId for metadata
			groupID, artifactID, _ := strings.Cut(name, ":")

			result = append(result, &extractor.Package{
				Name:     name,
				Version:  node.VersionKey.Version,
				PURLType: purl.TypeMaven,
				Metadata: &javalockfile.Metadata{
					ArtifactID:   artifactID,
					GroupID:      groupID,
					IsTransitive: node.Relation == "INDIRECT",
				},
				Locations: []string{path},
				Plugins:   []string{MavenDepsDevEnricherName},
			})
		}
	}

	if len(result) == 0 && len(pkgMap) > 0 {
		return nil, fmt.Errorf("no Maven dependencies resolved from deps.dev")
	}

	return result, nil
}
