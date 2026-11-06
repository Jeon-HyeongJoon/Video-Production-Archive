package sqlitebase

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Requirement: PER-002
func TestSQLiteRepositoriesShareOneConnectionLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	first, releaseFirst, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	second, releaseSecond, err := Acquire(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same SQLite path created independent connection pools")
	}
	if _, err := first.ExecContext(context.Background(), `CREATE TABLE shared_lifecycle(id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := releaseFirst(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ExecContext(context.Background(), `INSERT INTO shared_lifecycle(id) VALUES ('alive')`); err != nil {
		t.Fatalf("one repository close terminated the shared connection: %v", err)
	}
	if err := releaseSecond(); err != nil {
		t.Fatal(err)
	}
	if err := second.PingContext(context.Background()); err == nil {
		t.Fatal("last repository close left the shared connection open")
	}
}

func TestFixedWidthTimestampTextPreservesChronologicalOrder(t *testing.T) {
	wholeSecond := time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC)
	fractional := wholeSecond.Add(900 * time.Millisecond)
	nextSecond := wholeSecond.Add(time.Second)
	if strings.Compare(FormatTime(wholeSecond), FormatTime(fractional)) >= 0 ||
		strings.Compare(FormatTime(fractional), FormatTime(nextSecond)) >= 0 {
		t.Fatalf("SQLite text time order differs from chronological order: %q %q %q",
			FormatTime(wholeSecond), FormatTime(fractional), FormatTime(nextSecond))
	}
}
