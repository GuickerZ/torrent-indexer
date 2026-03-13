package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	handler "github.com/felipemarinho97/torrent-indexer/api"
	"github.com/felipemarinho97/torrent-indexer/cache"
	"github.com/felipemarinho97/torrent-indexer/logging"
	"github.com/felipemarinho97/torrent-indexer/magnet"
	"github.com/felipemarinho97/torrent-indexer/monitoring"
	"github.com/felipemarinho97/torrent-indexer/requester"
	"github.com/felipemarinho97/torrent-indexer/schema"
	goscrape "github.com/felipemarinho97/torrent-indexer/scrape"
	meilisearch "github.com/felipemarinho97/torrent-indexer/search"
	"github.com/felipemarinho97/torrent-indexer/utils"
	"gopkg.in/yaml.v3"
)

// FieldSelectorItem is a single extraction step: find an element, read an
// attribute or text, optionally apply a regex and/or a named parse function.
type FieldSelectorItem struct {
	// CSS selector relative to the current selection (empty = use current node).
	Selector string `yaml:"selector"`
	// HTML attribute to read (empty = use text content).
	Attr string `yaml:"attr,omitempty"`
	// Regex applied to the extracted value; first capture group is returned.
	// If there is no capture group the full match is returned.
	Regex string `yaml:"regex,omitempty"`
	// Mappings is an ordered list of pattern→value pairs used for enum-style
	// fields like audio. Each pattern is tested against the extracted text; the
	// value of the first match is returned. When Mappings is set, Regex is
	// still applied first (to narrow the input), then Mappings is applied.
	Mappings []AudioPattern `yaml:"mappings,omitempty"`
	// ParseFunction names a registered ParseFunc applied after all other steps.
	// Use this for obfuscated/encoded values (e.g. "starck_data_u").
	ParseFunction string `yaml:"parse_function,omitempty"`
	// Format is a Go time.Parse layout used when this item belongs to a "date" field.
	// Defaults to time.RFC3339, then "2006-01-02".
	Format string `yaml:"format,omitempty"`
}

// FieldSelector is an ordered list of FieldSelectorItems.
//
// For scalar fields (title, year, size, …) the engine tries each item in
// order and returns the first non-empty result.
//
// For slice fields (magnet_link, audio) the engine collects ALL results from
// ALL items.
//
//	magnet_link:
//	  - selector: "a[href^='magnet:']"
//	    attr: "href"
//	  - selector: "a[data-u]"
//	    attr: "data-u"
//	    parse_function: "starck_data_u"
type FieldSelector []FieldSelectorItem

// UnmarshalYAML implements yaml.Unmarshaler. A FieldSelector must always be
// declared as a YAML sequence (list of FieldSelectorItem mappings).
func (fs *FieldSelector) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("field selector must be a sequence (list), got kind %v", value.Kind)
	}
	var items []FieldSelectorItem
	if err := value.Decode(&items); err != nil {
		return err
	}
	*fs = items
	return nil
}

// AudioPattern maps a regex to a schema.Audio value.
type AudioPattern struct {
	// Pattern is a regex matched against the raw text.
	Pattern string `yaml:"pattern"`
	// Value is a schema.Audio constant, e.g. "Português", "Inglês", "Russo".
	Value string `yaml:"value"`
}

// FilesConfig describes how to extract a file list from a detail page.
// Each HTML node matched by Selector is parsed independently:
// NameRegex captures the file name (group 1), SizeRegex captures the size (group 1).
type FilesConfig struct {
	Selector  string `yaml:"selector"`
	NameRegex string `yaml:"name_regex"`
	SizeRegex string `yaml:"size_regex"`
}

// DetailPageConfig describes the optional second-fetch (post/detail page).
type DetailPageConfig struct {
	// Enabled controls whether a second HTTP fetch is made for each item.
	// When false, all fields must be extractable from the listing page itself.
	Enabled bool                     `yaml:"enabled"`
	Fields  map[string]FieldSelector `yaml:"fields"`
	Files   FilesConfig              `yaml:"files"`
}

// SelectorsConfig groups all CSS selectors for a definition.
type SelectorsConfig struct {
	// Item is the top-level list selector on the listing/search page.
	Item string `yaml:"item"`
	// Fields are extracted from each Item on the listing page.
	Fields map[string]FieldSelector `yaml:"fields"`
	// DetailPage configures extraction from the individual post page.
	DetailPage DetailPageConfig `yaml:"detail_page"`
}

// IndexerDefinition is the full YAML schema for a single generic indexer.
type IndexerDefinition struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
	URL   string `yaml:"url"`
	// SearchPattern is appended to URL for searches.
	// Use {query} as the placeholder: "?s={query}" or "index.php?s={query}"
	SearchPattern string `yaml:"search_pattern"`
	// PagePattern is used for path-based pagination.
	// Use {page} as the placeholder: "page/{page}" or "?paged={page}"
	PagePattern string          `yaml:"page_pattern"`
	Selectors   SelectorsConfig `yaml:"selectors"`
}

var whitespaceRe = regexp.MustCompile(`\s+`)

type genericEngine struct {
	def       IndexerDefinition
	indexer   *handler.Indexer
	redis     *cache.Redis
	metrics   *monitoring.Metrics
	req       *requester.Requster
	si        *meilisearch.SearchIndexer
	magnetAPI *magnet.MetadataClient
}

func (g *genericEngine) ID() string    { return g.def.ID }
func (g *genericEngine) Label() string { return g.def.Label }

func (g *genericEngine) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			g.metrics.IndexerDuration.WithLabelValues(g.def.Label).Observe(time.Since(start).Seconds())
			g.metrics.IndexerRequests.WithLabelValues(g.def.Label).Inc()
		}()

		ctx := r.Context()
		q := r.URL.Query().Get("q")
		page := r.URL.Query().Get("page")

		targetURL := g.buildURL(q, page)

		logging.InfoWithRequest(r).Str("target_url", targetURL).Msg("Processing generic indexer request")
		logging.InfoWithRequest(r).Str("target_url", targetURL).Msg(fmt.Sprintf("Processing %s indexer request", g.def.Label))

		resp, err := g.req.GetDocument(ctx, targetURL)
		if err != nil {
			handler.WriteError(w, r, http.StatusInternalServerError, err)
			g.metrics.IndexerErrors.WithLabelValues(g.def.Label).Inc()
			return
		}
		defer resp.Close()

		doc, err := goquery.NewDocumentFromReader(resp)
		if err != nil {
			handler.WriteError(w, r, http.StatusInternalServerError, err)
			g.metrics.IndexerErrors.WithLabelValues(g.def.Label).Inc()
			return
		}

		var indexedTorrents []schema.IndexedTorrent

		if g.def.Selectors.DetailPage.Enabled {
			// fetch all detail pages and extract from them
			var links []string
			postURLSel := g.def.Selectors.Fields["post_url"]
			doc.Find(g.def.Selectors.Item).Each(func(_ int, s *goquery.Selection) {
				link := extractScalarField(s, postURLSel)
				if link != "" {
					links = append(links, link)
				}
			})

			if len(links) == 0 {
				_ = g.req.ExpireDocument(ctx, targetURL)
				logging.DebugWithRequest(r).Str("target_url", targetURL).Msg("No item links found; expiring document in cache")
			}

			indexedTorrents = utils.ParallelFlatMap(links, func(link string) ([]schema.IndexedTorrent, error) {
				return g.extractTorrentsFromPage(ctx, link, targetURL)
			})
		} else {
			// extract everything directly from the listing page
			doc.Find(g.def.Selectors.Item).Each(func(_ int, s *goquery.Selection) {
				torrents := g.extractTorrentsFromDoc(ctx, s, targetURL)
				indexedTorrents = append(indexedTorrents, torrents...)
			})

			if len(indexedTorrents) == 0 {
				_ = g.req.ExpireDocument(ctx, targetURL)
			}
		}

		postProcessed := g.indexer.ApplyPostProcessors(r, indexedTorrents)
		handler.WriteResponse(w, r, postProcessed, indexedTorrents)
	}
}

// buildURL constructs the target URL for a given query and page.
func (g *genericEngine) buildURL(q, page string) string {
	base := g.def.URL

	if page != "" && g.def.PagePattern != "" {
		return base + strings.ReplaceAll(g.def.PagePattern, "{page}", page)
	}

	if q == "" {
		return base
	}

	return base + strings.ReplaceAll(g.def.SearchPattern, "{query}", url.QueryEscape(q))
}

// resolveURL resolves a possibly relative href against the base URL of the indexer.
func resolveURL(base, href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(href, "/")
}

// extractTorrentsFromPage fetches a detail page and extracts torrent entries.
func (g *genericEngine) extractTorrentsFromPage(ctx context.Context, link, referer string) ([]schema.IndexedTorrent, error) {
	resp, err := g.req.GetDocument(ctx, resolveURL(g.def.URL, link), referer)
	if err != nil {
		return nil, err
	}
	defer resp.Close()

	doc, err := goquery.NewDocumentFromReader(resp)
	if err != nil {
		return nil, err
	}

	return g.extractTorrentsFromDoc(ctx, doc.Selection, resolveURL(g.def.URL, link)), nil
}

// extractTorrentsFromDoc is the shared core that works on any *goquery.Selection
func (g *genericEngine) extractTorrentsFromDoc(ctx context.Context, sel *goquery.Selection, link string) []schema.IndexedTorrent {
	// Choose field map: detail page fields if detail mode, otherwise listing fields.
	var fields map[string]FieldSelector
	if g.def.Selectors.DetailPage.Enabled {
		fields = g.def.Selectors.DetailPage.Fields
	} else {
		fields = g.def.Selectors.Fields
	}

	title := extractScalarField(sel, fields["title"])

	// Pass the raw href through GetIMDBLink so both direct imdb.com URLs and
	// indirect patterns like opensubtitles "imdbid-NNNNN" are resolved.
	imdbRaw := extractScalarField(sel, fields["imdb"])
	imdbLink, _ := handler.GetIMDBLink(imdbRaw)

	// extract audio info using both built-in patterns and custom mappings
	audioTexts := extractSliceField(sel, fields["audio"])
	var audio []schema.Audio
	for _, text := range audioTexts {

		if len(text) > 0 {
			sep := handler.GetSeparator(text)
			langs_raw := strings.Split(text, sep)
			for _, lang := range langs_raw {
				lang = strings.TrimSpace(lang)
				a := schema.GetAudioFromString(lang)
				if a != nil {
					audio = append(audio, *a)
				} else if strings.TrimSpace(lang) != "" {
					logging.Warn().
						Str("language", lang).
						Msg("Unknown language detected")
					logging.Debug().Str("text", text).Msg("Unknown language detected from this text")
				}
			}
		}
	}
	if hasMappings(fields["audio"]) {
		for _, v := range extractMappingsField(sel, fields["audio"]) {
			audio = append(audio, schema.Audio(v))
		}
	}
	audio = utils.DeduplicateAudio(audio)

	sizeText := extractScalarField(sel, fields["size"])
	sizes := utils.StableUniq(handler.FindSizesFromText(sizeText))

	// magnet_link: collect ALL hrefs from ALL selector items (plain + encoded).
	var magnetLinks []string
	for _, href := range extractSliceField(sel, fields["magnet_link"]) {
		if utils.IsMagnetLink(href) {
			magnetLinks = append(magnetLinks, href)
		}
	}

	date := extractDateField(sel, fields["date"])
	year := extractScalarField(sel, fields["year"])
	if year == "" && date.Year() != 0 { // time.Time zero value has Year=1
		year = fmt.Sprintf("%d", date.Year())
	}

	files := extractFilesField(sel, g.def.Selectors.DetailPage.Files)

	htmlSeeds := extractIntField(sel, fields["seeds"])
	htmlPeers := extractIntField(sel, fields["peers"])

	type result struct {
		t   schema.IndexedTorrent
		idx int
	}
	ch := make(chan result, len(magnetLinks))

	for it, magnetLink := range magnetLinks {
		it, magnetLink := it, magnetLink
		go func() {
			parsed, err := magnet.ParseMagnetUri(magnetLink)
			if err != nil {
				logging.Error().Err(err).Str("magnet_link", magnetLink).Msg("Failed to parse magnet URI")
				ch <- result{idx: it}
				return
			}

			releaseTitle := parsed.DisplayName
			infoHash := parsed.InfoHash.String()
			trackers := parsed.Trackers
			magnetAudio := handler.GetAudioFromTitle(releaseTitle, audio)

			var seed, peer int
			if fields["seeds"] != nil && fields["peers"] != nil {
				seed = htmlSeeds
				peer = htmlPeers
			} else {
				peer, seed, err = goscrape.GetLeechsAndSeeds(ctx, g.redis, g.metrics, infoHash, trackers)
				if err != nil {
					logging.Error().Err(err).Str("info_hash", infoHash).Msg("Failed to get leechers and seeders")
				}
			}

			processedTitle := handler.ProcessTitle(title, magnetAudio)

			var mySize string
			if len(sizes) == len(magnetLinks) {
				mySize = sizes[it]
			}
			if mySize == "" && g.magnetAPI != nil {
				go func() { _, _ = g.magnetAPI.FetchMetadata(ctx, magnetLink) }()
			}

			ch <- result{
				idx: it,
				t: schema.IndexedTorrent{
					Title:         releaseTitle,
					OriginalTitle: processedTitle,
					Details:       link,
					Year:          year,
					IMDB:          imdbLink,
					Audio:         magnetAudio,
					MagnetLink:    magnetLink,
					Date:          date,
					InfoHash:      infoHash,
					Trackers:      trackers,
					LeechCount:    peer,
					SeedCount:     seed,
					Size:          mySize,
					Files:         files,
				},
			}
		}()
	}

	var results []schema.IndexedTorrent
	for range magnetLinks {
		r := <-ch
		if r.t.MagnetLink != "" {
			results = append(results, r.t)
		}
	}
	return results
}

// hasMappings reports whether any item in fs has Mappings defined.
func hasMappings(fs FieldSelector) bool {
	for _, item := range fs {
		if len(item.Mappings) > 0 {
			return true
		}
	}
	return false
}

// extractSingleItem extracts a string value from the given selection using one
// FieldSelectorItem: find the element → read attr or text → apply regex → apply
// parse_function. Text is always whitespace-normalised.
func extractSingleItem(s *goquery.Selection, item FieldSelectorItem) string {
	sel := s
	if item.Selector != "" {
		sel = s.Find(item.Selector)
	}

	var val string
	if item.Attr != "" {
		val, _ = sel.Attr(item.Attr)
		val = strings.TrimSpace(val)
	} else {
		val = strings.TrimSpace(whitespaceRe.ReplaceAllString(sel.Text(), " "))
	}

	if item.Regex != "" && val != "" {
		re, err := regexp.Compile(item.Regex)
		if err == nil {
			if m := re.FindStringSubmatch(val); len(m) > 1 {
				val = strings.TrimSpace(m[1])
			} else if m := re.FindString(val); m != "" {
				val = strings.TrimSpace(m)
			} else {
				val = ""
			}
		}
	}

	if item.ParseFunction != "" && val != "" {
		if fn, ok := parseFuncRegistry[item.ParseFunction]; ok {
			if transformed, err := fn(val); err == nil {
				val = transformed
			}
		} else if item.ParseFunction != "" {
			logging.Warn().Str("function", item.ParseFunction).Msg("Unknown parse function specified in FieldSelectorItem")
		}
	}

	return val
}

// extractMappingsFromText returns all mapping values whose patterns match text.
// Used for enum-like fields (audio) where a single text block can match several
// patterns at once.
func extractMappingsFromText(text string, mappings []AudioPattern) []string {
	var out []string
	for _, mp := range mappings {
		re, err := regexp.Compile(mp.Pattern)
		if err == nil && re.MatchString(text) {
			out = append(out, mp.Value)
		}
	}
	return out
}

// extractMappingsField collects all mapping matches across all nodes matched by
// all FieldSelectorItems that carry Mappings. Used for the "audio" field.
func extractMappingsField(s *goquery.Selection, fs FieldSelector) []string {
	var results []string
	for _, item := range fs {
		if len(item.Mappings) == 0 {
			continue
		}
		sel := s
		if item.Selector != "" {
			sel = s.Find(item.Selector)
		}
		sel.Each(func(_ int, node *goquery.Selection) {
			var text string
			if item.Attr != "" {
				text, _ = node.Attr(item.Attr)
			} else {
				text = strings.TrimSpace(whitespaceRe.ReplaceAllString(node.Text(), " "))
			}
			results = append(results, extractMappingsFromText(text, item.Mappings)...)
		})
	}
	return results
}

// extractScalarField tries each FieldSelectorItem in order and returns the
// first non-empty result. Used for singular fields: title, year, size, etc.
func extractScalarField(s *goquery.Selection, fs FieldSelector) string {
	for _, item := range fs {
		if val := extractSingleItem(s, item); val != "" {
			return val
		}
	}
	return ""
}

// extractIntField extracts a scalar string field and parses it as a base-10
// integer. Returns 0 if the field is absent or cannot be parsed.
func extractIntField(s *goquery.Selection, fs FieldSelector) int {
	raw := strings.TrimSpace(extractScalarField(s, fs))
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return v
}

// extractSliceField iterates ALL FieldSelectorItems and for each one finds ALL
// matching elements, collecting every non-empty result. Used for fields that
// return multiple values: magnet_link (multiple magnets per post), audio.
func extractSliceField(s *goquery.Selection, fs FieldSelector) []string {
	var results []string
	for _, item := range fs {
		sel := s
		if item.Selector != "" {
			sel = s.Find(item.Selector)
		}
		sel.Each(func(_ int, node *goquery.Selection) {
			// Use a copy of item with no Selector so extractSingleItem operates
			// on the already-found node directly.
			itemCopy := item
			itemCopy.Selector = ""
			if val := extractSingleItem(node, itemCopy); val != "" {
				results = append(results, val)
			}
		})
	}
	return results
}

// extractDateField parses the published date using the "date" FieldSelector.
// It calls extractScalarField, then tries the item's Format layout followed by
// RFC3339 and "2006-01-02" as fallbacks. When the FieldSelector is empty (no
// items) it falls back to reading OpenGraph / article meta tags.
func extractDateField(sel *goquery.Selection, fs FieldSelector) time.Time {
	if len(fs) == 0 {
		return handler.GetPublishedDateFromMeta(&goquery.Document{Selection: sel})
	}

	// Use extractScalarField so fallback items are tried automatically.
	raw := extractScalarField(sel, fs)
	if raw == "" {
		return time.Time{}
	}

	// Collect the Format from whichever item matched (first non-empty).
	var format string
	for _, item := range fs {
		if extractSingleItem(sel, item) != "" {
			format = item.Format
			break
		}
	}

	for _, layout := range []string{format, time.RFC3339, "2006-01-02"} {
		if layout == "" {
			continue
		}
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// extractFilesField collects all file entries using FilesConfig.
// Each node matched by cfg.Selector is parsed with NameRegex (group 1 → Path)
// and SizeRegex (group 1 → Size).
func extractFilesField(sel *goquery.Selection, cfg FilesConfig) []schema.File {
	if cfg.Selector == "" {
		return nil
	}
	var nameRe, sizeRe *regexp.Regexp
	if cfg.NameRegex != "" {
		nameRe = regexp.MustCompile(cfg.NameRegex)
	}
	if cfg.SizeRegex != "" {
		sizeRe = regexp.MustCompile(cfg.SizeRegex)
	}
	var files []schema.File
	sel.Find(cfg.Selector).Each(func(_ int, node *goquery.Selection) {
		text := strings.TrimSpace(whitespaceRe.ReplaceAllString(node.Text(), " "))
		if text == "" {
			return
		}
		var f schema.File
		if nameRe != nil {
			if m := nameRe.FindStringSubmatch(text); len(m) > 1 {
				f.Path = strings.TrimSpace(m[1])
			}
		} else {
			f.Path = text
		}
		if sizeRe != nil {
			if m := sizeRe.FindStringSubmatch(text); len(m) > 1 {
				f.Size = strings.TrimSpace(m[1])
			}
		}
		if f.Path != "" {
			files = append(files, f)
		}
	})
	return files
}
