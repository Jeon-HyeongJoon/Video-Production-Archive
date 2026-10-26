package catalog

import (
	"errors"
	"sort"
	"strings"
	"time"

	"videoarchive/internal/core/asset"
	"videoarchive/internal/core/mediaanalysis"
	"videoarchive/internal/core/vocabulary"
)

var (
	ErrInvalidRecord = errors.New("catalog record is incomplete or invalid")
	ErrInvalidQuery  = errors.New("catalog search query is invalid")
)

// CatalogRecord is an append-only human or Agent-authored description revision
// for one OriginalAsset. It never owns or changes a file path.
type CatalogRecord struct {
	ID              string               `json:"id"`
	OriginalAssetID string               `json:"original_asset_id"`
	Revision        uint64               `json:"revision"`
	Title           string               `json:"title"`
	Description     string               `json:"description"`
	Keywords        []string             `json:"keywords"`
	AuthorType      vocabulary.ActorType `json:"author_type"`
	AuthorID        string               `json:"author_id"`
	RecordedAt      time.Time            `json:"recorded_at"`
}

func NewRecord(source CatalogRecord) (CatalogRecord, error) {
	record := source
	record.ID = strings.TrimSpace(record.ID)
	record.OriginalAssetID = strings.TrimSpace(record.OriginalAssetID)
	record.Title = strings.TrimSpace(record.Title)
	record.Description = strings.TrimSpace(record.Description)
	record.AuthorID = strings.TrimSpace(record.AuthorID)
	record.Keywords = normalizeKeywords(record.Keywords)
	if record.ID == "" || record.OriginalAssetID == "" || record.Revision == 0 || record.Title == "" || record.Description == "" ||
		(record.AuthorType != vocabulary.ActorHuman && record.AuthorType != vocabulary.ActorAgent) || record.AuthorID == "" || record.RecordedAt.IsZero() {
		return CatalogRecord{}, ErrInvalidRecord
	}
	return record, nil
}

type SearchQuery struct {
	Text              string
	VideoCodec        string
	MinDurationMillis int64
	MaxDurationMillis int64
}

func NormalizeQuery(source SearchQuery) (SearchQuery, error) {
	query := source
	query.Text = strings.ToLower(strings.TrimSpace(query.Text))
	query.VideoCodec = strings.ToLower(strings.TrimSpace(query.VideoCodec))
	if query.MinDurationMillis < 0 || query.MaxDurationMillis < 0 ||
		(query.MaxDurationMillis > 0 && query.MinDurationMillis > query.MaxDurationMillis) {
		return SearchQuery{}, ErrInvalidQuery
	}
	return query, nil
}

type SearchResult struct {
	Original   asset.OriginalAsset            `json:"original"`
	Metadata   CatalogRecord                  `json:"metadata"`
	Inspection *mediaanalysis.MediaInspection `json:"inspection,omitempty"`
}

func normalizeKeywords(source []string) []string {
	unique := make(map[string]bool, len(source))
	for _, keyword := range source {
		keyword = strings.TrimSpace(keyword)
		if keyword != "" {
			unique[keyword] = true
		}
	}
	keywords := make([]string, 0, len(unique))
	for keyword := range unique {
		keywords = append(keywords, keyword)
	}
	sort.Strings(keywords)
	return keywords
}
