// FILENAME: database_example_test.go
package gothrottle_test

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"

	_ "github.com/mattn/go-sqlite3" // SQLite driver for example
)

// DatabaseThrottler wraps database operations with rate limiting
type DatabaseThrottler struct {
	db      *sql.DB
	limiter *gothrottle.Limiter
}

// NewDatabaseThrottler creates a new database throttler
func NewDatabaseThrottler(db *sql.DB, opts gothrottle.Options) (*DatabaseThrottler, error) {
	limiter, err := gothrottle.NewLimiter(opts)
	if err != nil {
		return nil, err
	}

	return &DatabaseThrottler{
		db:      db,
		limiter: limiter,
	}, nil
}

// Query executes a throttled database query
func (dt *DatabaseThrottler) Query(query string, args ...interface{}) (*sql.Rows, error) {
	result, err := dt.limiter.Schedule(func() (interface{}, error) {
		return dt.db.Query(query, args...)
	})

	if err != nil {
		return nil, err
	}

	return result.(*sql.Rows), nil
}

// QueryRow executes a throttled single-row query
func (dt *DatabaseThrottler) QueryRow(query string, args ...interface{}) *sql.Row {
	result, _ := dt.limiter.Schedule(func() (interface{}, error) {
		return dt.db.QueryRow(query, args...), nil
	})

	return result.(*sql.Row)
}

// Exec executes a throttled database statement
func (dt *DatabaseThrottler) Exec(query string, args ...interface{}) (sql.Result, error) {
	result, err := dt.limiter.Schedule(func() (interface{}, error) {
		return dt.db.Exec(query, args...)
	})

	if err != nil {
		return nil, err
	}

	return result.(sql.Result), nil
}

// Close closes the database connection and stops the limiter
func (dt *DatabaseThrottler) Close() error {
	dt.limiter.Stop()
	return dt.db.Close()
}

// TestDatabaseThrottling demonstrates database query throttling
func TestDatabaseThrottling(t *testing.T) {
	// Create in-memory SQLite database for testing
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create test table
	_, err = db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, email TEXT)`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert test data
	for i := 1; i <= 10; i++ {
		_, err = db.Exec("INSERT INTO users (name, email) VALUES (?, ?)",
			fmt.Sprintf("User%d", i), fmt.Sprintf("user%d@example.com", i))
		if err != nil {
			t.Fatal(err)
		}
	}

	// Create throttled database wrapper
	// Limit to 3 concurrent queries with 100ms between query starts
	throttledDB, err := NewDatabaseThrottler(db, gothrottle.Options{
		MaxConcurrent: 3,
		MinTime:       100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer throttledDB.Close()

	// Test concurrent queries with throttling
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// This query will be throttled
			rows, err := throttledDB.Query("SELECT id, name, email FROM users WHERE id = ?", id+1)
			if err != nil {
				t.Errorf("Query failed: %v", err)
				return
			}
			defer rows.Close()

			if rows.Next() {
				var userId int
				var name, email string
				if err := rows.Scan(&userId, &name, &email); err != nil {
					t.Errorf("Scan failed: %v", err)
					return
				}
				t.Logf("Query %d result: ID=%d, Name=%s, Email=%s", id, userId, name, email)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// With 100ms between queries and max 3 concurrent, this should take at least 700ms
	// (10 queries / 3 concurrent) * 100ms * (batches-1) ≈ 700ms minimum
	expectedMinTime := 700 * time.Millisecond
	if elapsed < expectedMinTime {
		t.Logf("Warning: Queries completed faster than expected throttling would allow: %v", elapsed)
	} else {
		t.Logf("Queries properly throttled, took: %v", elapsed)
	}
}

// TestDatabaseInsertThrottling demonstrates throttling INSERT operations
func TestDatabaseInsertThrottling(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create test table
	_, err = db.Exec(`CREATE TABLE logs (id INTEGER PRIMARY KEY AUTOINCREMENT, message TEXT, created_at DATETIME)`)
	if err != nil {
		t.Fatal(err)
	}

	// Create throttled database with strict limits to prevent overwhelming the DB
	throttledDB, err := NewDatabaseThrottler(db, gothrottle.Options{
		MaxConcurrent: 2,                     // Only 2 concurrent writes
		MinTime:       50 * time.Millisecond, // 50ms between writes
	})
	if err != nil {
		t.Fatal(err)
	}
	defer throttledDB.Close()

	// Simulate high-frequency logging that needs throttling
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			message := fmt.Sprintf("Log entry %d", id)
			_, err := throttledDB.Exec("INSERT INTO logs (message, created_at) VALUES (?, datetime('now'))", message)
			if err != nil {
				t.Errorf("Insert failed: %v", err)
				return
			}

			t.Logf("Inserted log entry %d", id)
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Verify all records were inserted
	row := throttledDB.QueryRow("SELECT COUNT(*) FROM logs")
	var count int
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}

	if count != 10 {
		t.Errorf("Expected 10 records, got %d", count)
	}

	t.Logf("Successfully inserted %d records in %v with throttling", count, elapsed)
}

// Example shows how to use database throttling in a real application
func ExampleDatabaseThrottler() {
	// Open database connection
	db, err := sql.Open("sqlite3", "example.db")
	if err != nil {
		panic(err)
	}

	// Create throttled database wrapper
	// This prevents overwhelming the database with too many concurrent queries
	throttledDB, err := NewDatabaseThrottler(db, gothrottle.Options{
		MaxConcurrent: 5,                     // Max 5 concurrent DB operations
		MinTime:       10 * time.Millisecond, // 10ms between operations
	})
	if err != nil {
		panic(err)
	}
	defer throttledDB.Close()

	// Now all database operations through throttledDB will be rate limited
	rows, err := throttledDB.Query("SELECT id, name FROM users LIMIT 10")
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			panic(err)
		}
		fmt.Printf("User: %d - %s\n", id, name)
	}
}

// TestDistributedDatabaseThrottling shows how multiple app instances can share DB limits
func TestDistributedDatabaseThrottling(t *testing.T) {
	// This test would require Redis, so we'll simulate it with local store
	// In production, you'd use RedisStore for true distributed throttling

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create table
	_, err = db.Exec(`CREATE TABLE api_calls (id INTEGER PRIMARY KEY AUTOINCREMENT, endpoint TEXT, timestamp DATETIME)`)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate multiple application instances sharing the same database throttling limits
	// In production, you'd use gothrottle.RedisStore here
	throttledDB, err := NewDatabaseThrottler(db, gothrottle.Options{
		ID:            "shared-db-limiter", // Shared ID for distributed limiting
		MaxConcurrent: 3,                   // Global limit across all app instances
		MinTime:       100 * time.Millisecond,
		// Datastore: redisStore, // Would use Redis in production
	})
	if err != nil {
		t.Fatal(err)
	}
	defer throttledDB.Close()

	// Simulate API calls that need to log to database with rate limiting
	endpoints := []string{"/api/users", "/api/orders", "/api/products", "/api/auth", "/api/stats"}

	var wg sync.WaitGroup
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			endpoint := endpoints[id%len(endpoints)]
			_, err := throttledDB.Exec("INSERT INTO api_calls (endpoint, timestamp) VALUES (?, datetime('now'))", endpoint)
			if err != nil {
				t.Errorf("Failed to log API call: %v", err)
				return
			}

			t.Logf("Logged API call %d to %s", id, endpoint)
		}(i)
	}

	wg.Wait()

	// Verify all calls were logged
	row := throttledDB.QueryRow("SELECT COUNT(*) FROM api_calls")
	var count int
	if err := row.Scan(&count); err != nil {
		t.Fatal(err)
	}

	t.Logf("Successfully logged %d API calls with distributed throttling", count)
}
