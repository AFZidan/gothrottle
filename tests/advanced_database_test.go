// FILENAME: advanced_database_test.go
package gothrottle_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"

	_ "github.com/mattn/go-sqlite3"
)

// WeightedDatabaseThrottler demonstrates using different weights for different operation types
type WeightedDatabaseThrottler struct {
	db      *sql.DB
	limiter *gothrottle.Limiter
}

func NewWeightedDatabaseThrottler(db *sql.DB, opts gothrottle.Options) (*WeightedDatabaseThrottler, error) {
	limiter, err := gothrottle.NewLimiter(opts)
	if err != nil {
		return nil, err
	}

	return &WeightedDatabaseThrottler{
		db:      db,
		limiter: limiter,
	}, nil
}

func (dt *WeightedDatabaseThrottler) Close() {
	_ = dt.limiter.Stop() // Ignore error in test cleanup
}

func (dt *WeightedDatabaseThrottler) ExecWithWeight(weight int, query string, args ...interface{}) (sql.Result, error) {
	result, err := dt.limiter.ScheduleWithOptions(func() (interface{}, error) {
		return dt.db.Exec(query, args...)
	}, 5, weight)
	if err != nil {
		return nil, err
	}
	return result.(sql.Result), nil
}

// TestWeightedDatabaseOperations demonstrates different weights for different database operations
func TestWeightedDatabaseOperations(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE weighted_ops (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT)`); err != nil {
		t.Fatal(err)
	}

	throttler, err := NewWeightedDatabaseThrottler(db, gothrottle.Options{
		MaxConcurrent: 3,
		MinTime:       time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer throttler.Close()

	if _, err := throttler.ExecWithWeight(3, "INSERT INTO weighted_ops (kind) VALUES (?)", "heavy"); err != nil {
		t.Fatalf("heavy operation failed: %v", err)
	}
	if _, err := throttler.ExecWithWeight(1, "INSERT INTO weighted_ops (kind) VALUES (?)", "light"); err != nil {
		t.Fatalf("light operation failed: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM weighted_ops").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("inserted rows = %d, want 2", count)
	}
}

// TestBatchProcessingWithThrottling shows how to process large datasets with rate limiting
func TestBatchProcessingWithThrottling(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE batches (id INTEGER PRIMARY KEY AUTOINCREMENT, batch_id INTEGER, value TEXT)`); err != nil {
		t.Fatal(err)
	}

	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 1,
		MinTime:       time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }()

	for batchID := 0; batchID < 3; batchID++ {
		batchID := batchID
		_, err := limiter.Schedule(func() (interface{}, error) {
			for item := 0; item < 5; item++ {
				if _, err := db.Exec("INSERT INTO batches (batch_id, value) VALUES (?, ?)", batchID, "value"); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("batch %d failed: %v", batchID, err)
		}
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM batches").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 15 {
		t.Fatalf("processed rows = %d, want 15", count)
	}
}

func TestLimiter_RejectsMalformedDatastoreIDs(t *testing.T) {
	store := gothrottle.NewLocalStore()

	_, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:        strings.Repeat("a", 513),
		Datastore: store,
	})
	if !errors.Is(err, gothrottle.ErrInvalidID) {
		t.Fatalf("oversized ID error = %v, want ErrInvalidID", err)
	}

	_, err = gothrottle.NewLimiter(gothrottle.Options{
		ID:        "bad\nid",
		Datastore: store,
	})
	if !errors.Is(err, gothrottle.ErrInvalidID) {
		t.Fatalf("control-character ID error = %v, want ErrInvalidID", err)
	}
}

// BenchmarkThrottledDatabaseOperations measures performance with throttling
func BenchmarkThrottledDatabaseOperations(b *testing.B) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	// Create table
	_, err = db.Exec(`CREATE TABLE benchmark_data (id INTEGER PRIMARY KEY, value TEXT)`)
	if err != nil {
		b.Fatal(err)
	}

	// Create throttler
	throttler, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 10,
		MinTime:       time.Millisecond,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = throttler.Stop() }() // Ignore error in test cleanup

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := throttler.Schedule(func() (interface{}, error) {
				return db.Exec("INSERT INTO benchmark_data (value) VALUES (?)", "test_value")
			})
			if err != nil {
				b.Error(err)
			}
		}
	})
}
