package engine

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	handler "github.com/felipemarinho97/torrent-indexer/api"
	"github.com/felipemarinho97/torrent-indexer/cache"
	"github.com/felipemarinho97/torrent-indexer/magnet"
	"github.com/felipemarinho97/torrent-indexer/monitoring"
	"github.com/felipemarinho97/torrent-indexer/requester"
	meilisearch "github.com/felipemarinho97/torrent-indexer/search"
	"gopkg.in/yaml.v3"
)

//go:embed definitions/*.yaml
var bundledDefinitions embed.FS

// Load reads all bundled definitions first, then overlays any *.yaml files
// found in customDir (if non-empty). A file in customDir whose base name matches
// a bundled file replaces it. Returns a slice of Engines ready for registration.
func Load(
	customDir string,
	indexer *handler.Indexer,
	redis *cache.Redis,
	metrics *monitoring.Metrics,
	req *requester.Requster,
	si *meilisearch.SearchIndexer,
	magnetAPI *magnet.MetadataClient,
) ([]Engine, error) {
	// Collect raw YAML bytes keyed by filename; customDir entries win.
	files := make(map[string][]byte)

	// 1. Bundled definitions.
	entries, err := fs.ReadDir(bundledDefinitions, "definitions")
	if err != nil {
		return nil, fmt.Errorf("reading bundled definitions: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := bundledDefinitions.ReadFile("definitions/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading bundled %s: %w", e.Name(), err)
		}
		files[e.Name()] = data
	}

	// 2. Overlay with user-provided definitions.
	if customDir != "" {
		dirEntries, err := os.ReadDir(customDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading extra definitions dir %q: %w", customDir, err)
		}
		for _, e := range dirEntries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(customDir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", e.Name(), err)
			}
			files[e.Name()] = data // replaces bundled definition if same filename
		}
	}

	// Parse all collected definitions.
	rawDefs := make(map[string]IndexerDefinition)
	for name, data := range files {
		var def IndexerDefinition
		if err := yaml.Unmarshal(data, &def); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		rawDefs[def.ID] = def
	}

	// Build engines, applying env var URL overrides and skipping template definitions.
	var engines []Engine
	for _, def := range rawDefs {
		resolved := def

		envKey := fmt.Sprintf("INDEXER_%s_URL", strings.ToUpper(strings.ReplaceAll(resolved.ID, "-", "_")))
		if v := os.Getenv(envKey); v != "" {
			resolved.URL = v
		}

		if resolved.URL == "" {
			continue
		}

		engines = append(engines, &genericEngine{
			def:       resolved,
			indexer:   indexer,
			redis:     redis,
			metrics:   metrics,
			req:       req,
			si:        si,
			magnetAPI: magnetAPI,
		})
	}
	return engines, nil
}
