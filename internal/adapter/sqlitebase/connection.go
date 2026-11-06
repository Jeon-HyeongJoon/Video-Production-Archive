package sqlitebase

import (
	"database/sql"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const timestampLayout = "2006-01-02T15:04:05.000000000Z"

// FormatTime keeps SQLite text timestamps in one fixed-width UTC form so
// ORDER BY has the same ordering as time.Time. RFC3339Nano itself is not safe
// for this because it trims the fractional second to a variable width.
func FormatTime(value time.Time) string { return value.UTC().Format(timestampLayout) }

type sharedConnection struct {
	db         *sql.DB
	references int
}

var connections = struct {
	sync.Mutex
	byPath map[string]*sharedConnection
}{byPath: make(map[string]*sharedConnection)}

// Acquire returns the one process-wide SQLite connection pool for a database
// path. The release function must be called exactly once by every owner.
func Acquire(path string) (*sql.DB, func() error, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	absolute = filepath.Clean(absolute)
	connections.Lock()
	defer connections.Unlock()
	if existing := connections.byPath[absolute]; existing != nil {
		existing.references++
		return existing.db, release(absolute, existing), nil
	}
	db, err := sql.Open("sqlite", absolute+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, err
	}
	// A single connection makes all repository transactions share one lock and
	// prevents independent pools from surfacing SQLITE_BUSY as a domain conflict.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	entry := &sharedConnection{db: db, references: 1}
	connections.byPath[absolute] = entry
	return db, release(absolute, entry), nil
}

func release(path string, expected *sharedConnection) func() error {
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() {
			connections.Lock()
			defer connections.Unlock()
			current := connections.byPath[path]
			if current == nil || current != expected {
				return
			}
			current.references--
			if current.references == 0 {
				delete(connections.byPath, path)
				closeErr = current.db.Close()
			}
		})
		return closeErr
	}
}
