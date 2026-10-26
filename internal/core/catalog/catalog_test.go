package catalog

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"videoarchive/internal/core/vocabulary"
)

// Requirement: CAT-001
func TestCatalogRecordKeepsVersionedDescriptionAndAuthorOrigin(t *testing.T) {
	record, err := NewRecord(CatalogRecord{
		ID: " RECORD-1 ", OriginalAssetID: " ASSET-1 ", Revision: 1,
		Title: " 2026 가을 행사 ", Description: " 전체 행사 기록 영상 ", Keywords: []string{"행사", " 기록 ", "행사"},
		AuthorType: vocabulary.ActorHuman, AuthorID: " operator-1 ", RecordedAt: time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != "RECORD-1" || record.OriginalAssetID != "ASSET-1" || record.Revision != 1 ||
		record.Title != "2026 가을 행사" || record.Description != "전체 행사 기록 영상" ||
		!reflect.DeepEqual(record.Keywords, []string{"기록", "행사"}) || record.AuthorID != "operator-1" {
		t.Fatalf("catalog metadata was not normalized: %+v", record)
	}
	invalid := record
	invalid.Description = ""
	if _, err := NewRecord(invalid); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("description-free catalog record accepted: %v", err)
	}
}
