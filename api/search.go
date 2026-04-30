package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/felipemarinho97/torrent-indexer/schema"
	meilisearch "github.com/felipemarinho97/torrent-indexer/search"
)

// MeilisearchHandler handles HTTP requests for Meilisearch integration.
type MeilisearchHandler struct {
	Module *meilisearch.SearchIndexer
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status    string                 `json:"status"`
	Service   string                 `json:"service"`
	Details   map[string]interface{} `json:"details,omitempty"`
	Timestamp string                 `json:"timestamp"`
}

// StatsResponse represents the stats endpoint response
type StatsResponse struct {
	Status            string           `json:"status"`
	NumberOfDocuments int64            `json:"numberOfDocuments"`
	IsIndexing        bool             `json:"isIndexing"`
	FieldDistribution map[string]int64 `json:"fieldDistribution"`
	Service           string           `json:"service"`
}

// NewMeilisearchHandler creates a new instance of MeilisearchHandler.
func NewMeilisearchHandler(module *meilisearch.SearchIndexer) *MeilisearchHandler {
	return &MeilisearchHandler{Module: module}
}

// MultiSearchRequest represents the request body for multiple searches
type MultiSearchRequest struct {
	Queries []string `json:"queries"`
	Limit   int      `json:"limit,omitempty"`
}

// SearchTorrentHandler handles the searching of torrent items.
func (h *MeilisearchHandler) SearchTorrentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var queries []string
	limit := 10

	if r.Method == http.MethodGet {
		queries = r.URL.Query()["q"]
		if len(queries) == 0 {
			// needed for prowlar/jackett empty queries
			queries = []string{time.Now().Format("2006-01-02")}
		}
		if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
			if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
				limit = l
			}
		}
	} else if r.Method == http.MethodPost {
		var req MultiSearchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		queries = req.Queries
		if req.Limit > 0 {
			limit = req.Limit
		}
	}

	allResultsArrays, err := h.Module.MultiSearchTorrent(queries, limit)
	if err != nil {
		http.Error(w, "Failed to perform search", http.StatusInternalServerError)
		return
	}

	var allResults []schema.IndexedTorrent
	for _, results := range allResultsArrays {
		allResults = append(allResults, results...)
	}

	// Format response to match indexers structure
	response := map[string]interface{}{
		"results": allResults,
		"count":   len(allResults),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// HealthHandler provides a health check endpoint for Meilisearch.
func (h *MeilisearchHandler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Check if Meilisearch is healthy
	isHealthy := h.Module.IsHealthy()

	response := HealthResponse{
		Service:   "meilisearch",
		Timestamp: getCurrentTimestamp(),
	}

	if isHealthy {
		// Try to get additional stats for more detailed health info
		stats, err := h.Module.GetStats()
		if err == nil {
			response.Status = "healthy"
			response.Details = map[string]interface{}{
				"documents": stats.NumberOfDocuments,
				"indexing":  stats.IsIndexing,
			}
			w.WriteHeader(http.StatusOK)
		} else {
			// Service is up but can't get stats
			response.Status = "degraded"
			response.Details = map[string]interface{}{
				"error": "Could not retrieve stats",
			}
			w.WriteHeader(http.StatusOK)
		}
	} else {
		// Service is down
		response.Status = "unhealthy"
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// StatsHandler provides detailed statistics about the Meilisearch index.
func (h *MeilisearchHandler) StatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Get detailed stats from Meilisearch
	stats, err := h.Module.GetStats()
	if err != nil {
		// Check if it's a connectivity issue
		if !h.Module.IsHealthy() {
			http.Error(w, "Meilisearch service is unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "Failed to retrieve statistics", http.StatusInternalServerError)
		return
	}

	response := StatsResponse{
		Status:            "healthy",
		Service:           "meilisearch",
		NumberOfDocuments: stats.NumberOfDocuments,
		IsIndexing:        stats.IsIndexing,
		FieldDistribution: stats.FieldDistribution,
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// getCurrentTimestamp returns the current timestamp in RFC3339 format
func getCurrentTimestamp() string {
	return time.Now().Format(time.RFC3339)
}
