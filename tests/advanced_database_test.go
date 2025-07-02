// FILENAME: advanced_database_example_test.go
package gothrottle_test

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"gothrottle"

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

// SelectQuery - lightweight operation (weight 1)
func (wdt *WeightedDatabaseThrottler) SelectQuery(query string, args ...interface{}) (*sql.Rows, error) {
	result, err := wdt.limiter.ScheduleWithOptions(func() (interface{}, error) {
		return wdt.db.Query(query, args...)
	}, 5, 1) // Normal priority, weight 1

	if err != nil {
		return nil, err
	}
	return result.(*sql.Rows), nil
}

// ComplexQuery - heavy operation (weight 3)
func (wdt *WeightedDatabaseThrottler) ComplexQuery(query string, args ...interface{}) (*sql.Rows, error) {
	result, err := wdt.limiter.ScheduleWithOptions(func() (interface{}, error) {
		return wdt.db.Query(query, args...)
	}, 7, 3) // Higher priority, weight 3 (uses more resources)

	if err != nil {
		return nil, err
	}
	return result.(*sql.Rows), nil
}

// Insert - medium operation (weight 2)
func (wdt *WeightedDatabaseThrottler) Insert(query string, args ...interface{}) (sql.Result, error) {
	result, err := wdt.limiter.ScheduleWithOptions(func() (interface{}, error) {
		return wdt.db.Exec(query, args...)
	}, 6, 2) // Medium priority, weight 2

	if err != nil {
		return nil, err
	}
	return result.(sql.Result), nil
}

// BulkInsert - very heavy operation (weight 5)
func (wdt *WeightedDatabaseThrottler) BulkInsert(queries []string, args [][]interface{}) error {
	_, err := wdt.limiter.ScheduleWithOptions(func() (interface{}, error) {
		tx, err := wdt.db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()

		for i, query := range queries {
			_, err := tx.Exec(query, args[i]...)
			if err != nil {
				return nil, err
			}
		}

		return tx.Commit(), nil
	}, 10, 5) // Highest priority, weight 5 (very resource intensive)

	return err
}

func (wdt *WeightedDatabaseThrottler) Close() error {
	wdt.limiter.Stop()
	return wdt.db.Close()
}

// TestWeightedDatabaseOperations demonstrates different weights for different DB operations
func TestWeightedDatabaseOperations(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create tables
	_, err = db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, email TEXT, created_at DATETIME)`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER, amount DECIMAL, created_at DATETIME)`)
	if err != nil {
		t.Fatal(err)
	}

	// Create weighted throttler - max capacity of 10 units
	wdt, err := NewWeightedDatabaseThrottler(db, gothrottle.Options{
		MaxConcurrent: 10, // 10 "units" of database capacity
		MinTime:       50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wdt.Close()

	var wg sync.WaitGroup
	start := time.Now()

	// Mix of different operations with different weights
	operations := []func(){
		// Light SELECT queries (weight 1 each) - 4 total weight
		func() {
			defer wg.Done()
			rows, err := wdt.SelectQuery("SELECT COUNT(*) FROM users")
			if err != nil {
				t.Errorf("Select query failed: %v", err)
				return
			}
			defer rows.Close()
			t.Log("Executed light SELECT query")
		},
		func() {
			defer wg.Done()
			rows, err := wdt.SelectQuery("SELECT COUNT(*) FROM orders")
			if err != nil {
				t.Errorf("Select query failed: %v", err)
				return
			}
			defer rows.Close()
			t.Log("Executed light SELECT query")
		},

		// Medium INSERT operations (weight 2 each) - 4 total weight
		func() {
			defer wg.Done()
			_, err := wdt.Insert("INSERT INTO users (name, email, created_at) VALUES (?, ?, datetime('now'))",
				"John Doe", "john@example.com")
			if err != nil {
				t.Errorf("Insert failed: %v", err)
				return
			}
			t.Log("Executed INSERT operation")
		},
		func() {
			defer wg.Done()
			_, err := wdt.Insert("INSERT INTO users (name, email, created_at) VALUES (?, ?, datetime('now'))",
				"Jane Smith", "jane@example.com")
			if err != nil {
				t.Errorf("Insert failed: %v", err)
				return
			}
			t.Log("Executed INSERT operation")
		},

		// Heavy complex query (weight 3) - 3 total weight
		func() {
			defer wg.Done()
			rows, err := wdt.ComplexQuery(`
				SELECT u.name, COUNT(o.id) as order_count, AVG(o.amount) as avg_amount
				FROM users u 
				LEFT JOIN orders o ON u.id = o.user_id 
				GROUP BY u.id, u.name
			`)
			if err != nil {
				t.Errorf("Complex query failed: %v", err)
				return
			}
			defer rows.Close()
			t.Log("Executed complex JOIN query")
		},

		// Very heavy bulk operation (weight 5) - 5 total weight
		func() {
			defer wg.Done()
			queries := []string{
				"INSERT INTO orders (user_id, amount, created_at) VALUES (?, ?, datetime('now'))",
				"INSERT INTO orders (user_id, amount, created_at) VALUES (?, ?, datetime('now'))",
				"INSERT INTO orders (user_id, amount, created_at) VALUES (?, ?, datetime('now'))",
			}
			args := [][]interface{}{
				{1, 99.99},
				{2, 149.99},
				{1, 79.99},
			}
			err := wdt.BulkInsert(queries, args)
			if err != nil {
				t.Errorf("Bulk insert failed: %v", err)
				return
			}
			t.Log("Executed bulk INSERT operation")
		},
	}

	// Start all operations concurrently
	for _, op := range operations {
		wg.Add(1)
		go op()
	}

	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("All weighted database operations completed in: %v", elapsed)

	// Verify data was inserted
	var userCount, orderCount int
	db.QueryRow("SELECT COUNT(*) FROM users").Scan(&userCount)
	db.QueryRow("SELECT COUNT(*) FROM orders").Scan(&orderCount)

	t.Logf("Final counts - Users: %d, Orders: %d", userCount, orderCount)
}

// TestBatchProcessingWithThrottling shows how to process large datasets with rate limiting
func TestBatchProcessingWithThrottling(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create test table
	_, err = db.Exec(`CREATE TABLE batch_data (id INTEGER PRIMARY KEY, value TEXT, processed_at DATETIME)`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert test data to process
	for i := 1; i <= 50; i++ {
		_, err = db.Exec("INSERT INTO batch_data (value) VALUES (?)", fmt.Sprintf("data_%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}

	// Create throttler for batch processing
	throttler, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 5,                     // Process max 5 records concurrently
		MinTime:       20 * time.Millisecond, // 20ms between batches
	})
	if err != nil {
		t.Fatal(err)
	}
	defer throttler.Stop()

	// Process data in batches with throttling
	batchSize := 10
	var wg sync.WaitGroup
	start := time.Now()

	for offset := 0; offset < 50; offset += batchSize {
		wg.Add(1)
		go func(batchOffset int) {
			defer wg.Done()

			_, err := throttler.Schedule(func() (interface{}, error) {
				// Simulate processing a batch of records
				rows, err := db.Query("SELECT id, value FROM batch_data LIMIT ? OFFSET ?", batchSize, batchOffset)
				if err != nil {
					return nil, err
				}
				defer rows.Close()

				var processedIds []int
				for rows.Next() {
					var id int
					var value string
					if err := rows.Scan(&id, &value); err != nil {
						return nil, err
					}

					// Simulate processing time
					time.Sleep(10 * time.Millisecond)

					// Update as processed
					_, err := db.Exec("UPDATE batch_data SET processed_at = datetime('now') WHERE id = ?", id)
					if err != nil {
						return nil, err
					}

					processedIds = append(processedIds, id)
				}

				return processedIds, nil
			})

			if err != nil {
				t.Errorf("Batch processing failed: %v", err)
				return
			}

			t.Logf("Processed batch starting at offset %d", batchOffset)
		}(offset)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Verify all records were processed
	var processedCount int
	db.QueryRow("SELECT COUNT(*) FROM batch_data WHERE processed_at IS NOT NULL").Scan(&processedCount)

	if processedCount != 50 {
		t.Errorf("Expected 50 processed records, got %d", processedCount)
	}

	t.Logf("Batch processing completed: %d records in %v", processedCount, elapsed)
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
	defer throttler.Stop()

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
