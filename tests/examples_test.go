// FILENAME: example_test.go
package gothrottle_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/AFZidan/gothrottle"

	"github.com/go-redis/redis/v8"
)

// Example demonstrates basic usage with LocalStore
func ExampleLimiter_local() {
	// Create a limiter with local storage
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 2,
		MinTime:       100 * time.Millisecond,
	})
	if err != nil {
		panic(err)
	}
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

	// Schedule some jobs
	for i := 0; i < 5; i++ {
		i := i // capture loop variable
		result, err := limiter.Schedule(func() (interface{}, error) {
			fmt.Printf("Executing job %d\n", i)
			time.Sleep(50 * time.Millisecond)
			return fmt.Sprintf("Result %d", i), nil
		})
		if err != nil {
			fmt.Printf("Job %d failed: %v\n", i, err)
		} else {
			fmt.Printf("Job %d completed: %v\n", i, result)
		}
	}
}

// Example demonstrates usage with RedisStore
func ExampleLimiter_redis() {
	// Create Redis client
	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})

	// Create Redis store
	store, err := gothrottle.NewRedisStore(rdb)
	if err != nil {
		panic(err)
	}

	// Create limiter with Redis storage
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		ID:            "my-limiter",
		MaxConcurrent: 3,
		MinTime:       200 * time.Millisecond,
		Datastore:     store,
	})
	if err != nil {
		panic(err)
	}
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

	// Schedule jobs that will be rate limited across multiple instances
	for i := 0; i < 3; i++ {
		i := i
		result, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
			fmt.Printf("Executing distributed job %d\n", i)
			return fmt.Sprintf("Distributed result %d", i), nil
		}, 10, 1) // Priority 10, weight 1
		if err != nil {
			fmt.Printf("Distributed job %d failed: %v\n", i, err)
		} else {
			fmt.Printf("Distributed job %d completed: %v\n", i, result)
		}
	}
}

// TestLimiter_Wrap demonstrates the Wrap functionality
func TestLimiter_Wrap(t *testing.T) {
	limiter, err := gothrottle.NewLimiter(gothrottle.Options{
		MaxConcurrent: 1,
		MinTime:       100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limiter.Stop() }() // Ignore error in test cleanup

	// Create a wrapped function
	wrappedFn := limiter.Wrap(func() (interface{}, error) {
		return "wrapped result", nil
	})

	// Call the wrapped function
	result, err := wrappedFn()
	if err != nil {
		t.Errorf("Wrapped function failed: %v", err)
	}
	if result != "wrapped result" {
		t.Errorf("Expected 'wrapped result', got %v", result)
	}
}
