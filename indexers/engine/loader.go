package engine

import (
	"fmt"
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

// LoadFromDir reads every *.yaml file in dir, resolves `extends` references,
// and returns a slice of GenericEngines ready for registration.
func LoadFromDir(
	dir string,
	indexer *handler.Indexer,
	redis *cache.Redis,
	metrics *monitoring.Metrics,
	req *requester.Requster,
	si *meilisearch.SearchIndexer,
	magnetAPI *magnet.MetadataClient,
) ([]Engine, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading indexers dir %q: %w", dir, err)
	}

	// First pass: load all raw definitions indexed by ID.
	rawDefs := make(map[string]IndexerDefinition)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		var def IndexerDefinition
		if err := yaml.Unmarshal(data, &def); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", entry.Name(), err)
		}
		rawDefs[def.ID] = def
	}

	// Second pass: resolve extends, compile patterns, build engines.
	var engines []Engine
	for _, def := range rawDefs {
		resolved := def

		// Allow env var override for the base URL.
		envKey := fmt.Sprintf("INDEXER_%s_URL", strings.ToUpper(strings.ReplaceAll(resolved.ID, "-", "_")))
		if v := os.Getenv(envKey); v != "" {
			resolved.URL = v
		}

		// Skip base/template definitions that have no URL configured.
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
