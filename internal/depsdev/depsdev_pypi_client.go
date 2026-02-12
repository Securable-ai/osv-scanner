// Package depsdev contains clients and utilities for the deps.dev API.
package depsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
)

// DepsDevDependencyGraph is the response from the deps.dev dependencies API.
type DepsDevDependencyGraph struct {
	Nodes []DepsDevNode `json:"nodes"`
	Edges []DepsDevEdge `json:"edges"`
}

// DepsDevNode represents a single package in the dependency graph.
type DepsDevNode struct {
	VersionKey DepsDevVersionKey `json:"versionKey"`
	Bundled    bool              `json:"bundled"`
	Relation   string            `json:"relation"` // SELF, DIRECT, INDIRECT
	Errors     []string          `json:"errors"`
}

// DepsDevVersionKey identifies a package version.
type DepsDevVersionKey struct {
	System  string `json:"system"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// DepsDevEdge represents a dependency relationship.
type DepsDevEdge struct {
	FromNode    int    `json:"fromNode"`
	ToNode      int    `json:"toNode"`
	Requirement string `json:"requirement"`
}

// DepsDevRESTClient fetches pre-computed dependency graphs from the deps.dev REST API.
// It supports any system (pypi, maven, npm, etc.) via the system field.
type DepsDevRESTClient struct {
	baseURL string
	system  string // e.g. "pypi", "maven"
	mu      sync.Mutex
	cache   map[string]*DepsDevDependencyGraph
}

// PyPIDepsDevClient is an alias for backward compatibility.
type PyPIDepsDevClient = DepsDevRESTClient

// NewPyPIDepsDevClient creates a new client for PyPI dependencies via the deps.dev REST API.
func NewPyPIDepsDevClient(baseURL string) *DepsDevRESTClient {
	return &DepsDevRESTClient{
		baseURL: baseURL,
		system:  "pypi",
		cache:   make(map[string]*DepsDevDependencyGraph),
	}
}

// NewMavenDepsDevClient creates a new client for Maven dependencies via the deps.dev REST API.
func NewMavenDepsDevClient(baseURL string) *DepsDevRESTClient {
	return &DepsDevRESTClient{
		baseURL: baseURL,
		system:  "maven",
		cache:   make(map[string]*DepsDevDependencyGraph),
	}
}

// GetDependencies fetches the pre-computed dependency graph for a package version.
// This is a single HTTP GET that returns the full transitive dependency tree.
func (c *DepsDevRESTClient) GetDependencies(ctx context.Context, name, version string) (*DepsDevDependencyGraph, error) {
	cacheKey := c.system + "/" + name + "@" + version

	c.mu.Lock()
	if cached, ok := c.cache[cacheKey]; ok {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	// Build URL: {baseURL}/v3/systems/{system}/packages/{name}/versions/{version}:dependencies
	reqURL := fmt.Sprintf("%s/v3/systems/%s/packages/%s/versions/%s:dependencies",
		c.baseURL,
		c.system,
		url.PathEscape(name),
		url.PathEscape(version),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deps.dev API request failed for %s@%s: %w", name, version, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("deps.dev API returned %d for %s@%s: %s", resp.StatusCode, name, version, string(body))
	}

	var graph DepsDevDependencyGraph
	if err := json.NewDecoder(resp.Body).Decode(&graph); err != nil {
		return nil, fmt.Errorf("failed to decode deps.dev response for %s@%s: %w", name, version, err)
	}

	c.mu.Lock()
	c.cache[cacheKey] = &graph
	c.mu.Unlock()

	return &graph, nil
}
