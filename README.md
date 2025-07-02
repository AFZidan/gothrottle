# GoThrottle

A Go package for request throttling and rate limiting, heavily inspired by the Node.js [bottleneck](https://www.npmjs.com/package/bottleneck) package.

## Features

- **Local and Distributed Rate Limiting**: Supports both in-memory (LocalStore) and Redis-based (RedisStore) backends
- **Configurable Limits**: Set maximum concurrent jobs and minimum time between jobs
- **Priority Queue**: Jobs are executed based on priority
- **Atomic Operations**: Redis operations use Lua scripts to prevent race conditions
- **Easy Integration**: Simple API for wrapping existing functions

## Installation

```bash
go get github.com/AFZidan/gothrottle
```

## Quick Start

### Local Rate Limiting

```go
package main

import (
    "fmt"
    "time"
    "github.com/AFZidan/gothrottle"
)

func main() {
    // Create a limiter with local storage
    limiter, err := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 2,                    // Max 2 concurrent jobs
        MinTime:       100 * time.Millisecond, // 100ms between jobs
    })
    if err != nil {
        panic(err)
    }
    defer limiter.Stop()

    // Schedule a job
    result, err := limiter.Schedule(func() (interface{}, error) {
        // Your work here
        return "Hello, World!", nil
    })
    
    fmt.Println(result) // "Hello, World!"
}
```

### Distributed Rate Limiting with Redis

```go
package main

import (
    "time"
    "github.com/AFZidan/gothrottle"
    "github.com/go-redis/redis/v8"
)

func main() {
    // Create Redis client
    rdb := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
    })

    // Create Redis store
    store, err := gothrottle.NewRedisStore(rdb)
    if err != nil {
        panic(err)
    }

    // Create limiter with Redis backend
    limiter, err := gothrottle.NewLimiter(gothrottle.Options{
        ID:            "my-distributed-limiter", // Required for Redis
        MaxConcurrent: 5,
        MinTime:       200 * time.Millisecond,
        Datastore:     store,
    })
    if err != nil {
        panic(err)
    }
    defer limiter.Stop()

    // This limiter will now coordinate with other instances
    // using the same Redis store and limiter ID
}
```

## API Reference

### Options

```go
type Options struct {
    ID            string        // Unique ID for the limiter (required for Redis)
    MaxConcurrent int           // Maximum concurrent jobs (0 = unlimited)
    MinTime       time.Duration // Minimum time between jobs
    Datastore     Datastore     // Storage backend (nil = LocalStore)
}
```

### Limiter Methods

#### `NewLimiter(opts Options) (*Limiter, error)`

Creates a new limiter instance.

#### `Schedule(task func() (interface{}, error)) (interface{}, error)`

Schedules a job with default priority (5) and weight (1). Blocks until completion.

#### `ScheduleWithOptions(task func() (interface{}, error), priority, weight int) (interface{}, error)`

Schedules a job with custom priority and weight. Higher priority jobs run first.

#### `Wrap(fn func() (interface{}, error)) func() (interface{}, error)`

Returns a wrapped version of the function that applies rate limiting.

#### `Stop() error`

Stops the limiter and cleans up resources.

### Storage Backends

#### LocalStore

In-memory storage for single-instance applications. This is the default when no `Datastore` is specified.

```go
store := gothrottle.NewLocalStore()
```

#### RedisStore

Redis-based storage for distributed rate limiting across multiple application instances.

```go
rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
store, err := gothrottle.NewRedisStore(rdb)
```

## Architecture

The package is built around a `Datastore` interface that allows pluggable storage backends:

```go
type Datastore interface {
    Request(limiterID string, weight int, opts Options) (canRun bool, waitTime time.Duration, err error)
    RegisterDone(limiterID string, weight int) error
    Disconnect() error
}
```

- **LocalStore**: Uses Go mutexes and in-memory state
- **RedisStore**: Uses atomic Lua scripts for race-condition-free distributed coordination

## Project Structure

```text
gothrottle/
├── datastore.go      # Datastore interface definition
├── options.go        # Configuration options
├── job.go           # Job struct and priority queue
├── local_store.go   # In-memory storage implementation
├── redis_store.go   # Redis-based storage implementation
├── limiter.go       # Main Limiter struct and logic
├── errors.go        # Common error definitions
├── tests/           # Test files
│   ├── examples_test.go            # Basic usage examples
│   ├── limiter_test.go             # Core limiter unit tests
│   ├── integration_test.go         # Integration tests and benchmarks
│   ├── database_test.go            # Database throttling tests
│   └── advanced_database_test.go   # Advanced DB operations with weights
├── go.mod           # Go module definition
├── go.sum           # Go module checksums
└── README.md        # This file
```

## Examples

See `tests/examples_test.go` for more detailed examples of usage patterns.

## Running Tests

```bash
# Run all tests
go test ./tests/... -v

# Run benchmarks
go test ./tests/... -bench=. -benchmem

# Test a specific function
go test ./tests/... -run TestLimiter_MaxConcurrent -v
```

## License

MIT License - see LICENSE file for details.

## Database Query Throttling

**GoThrottle** is excellent for throttling database operations to prevent overwhelming your database with too many concurrent queries. This is especially useful for:

- **Rate limiting API database calls**
- **Batch processing large datasets**  
- **Preventing database connection pool exhaustion**
- **Distributed rate limiting across multiple application instances**

### Basic Database Throttling

```go
package main

import (
    "database/sql"
    "gothrottle"
    _ "github.com/lib/pq" // PostgreSQL driver
)

// DatabaseThrottler wraps database operations with rate limiting
type DatabaseThrottler struct {
    db      *sql.DB
    limiter *gothrottle.Limiter
}

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

func main() {
    db, _ := sql.Open("postgres", "connection_string")
    
    // Limit to 5 concurrent queries with 10ms between query starts
    throttledDB, _ := NewDatabaseThrottler(db, gothrottle.Options{
        MaxConcurrent: 5,
        MinTime:       10 * time.Millisecond,
    })
    defer throttledDB.Close()
    
    // Now all queries through throttledDB will be rate limited
    rows, err := throttledDB.Query("SELECT * FROM users WHERE active = ?", true)
    // ... handle results
}
```

### Weighted Database Operations

Different database operations can have different resource costs. You can assign weights:

```go
// Light SELECT queries (weight 1)
rows, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
    return db.Query("SELECT id FROM users")
}, 5, 1) // Priority 5, Weight 1

// Heavy analytical queries (weight 5)  
rows, err := limiter.ScheduleWithOptions(func() (interface{}, error) {
    return db.Query("SELECT COUNT(*) FROM large_table GROUP BY complex_column")
}, 10, 5) // Priority 10, Weight 5

// With MaxConcurrent: 10, you can run either:
// - 10 light queries simultaneously, OR  
// - 2 heavy queries simultaneously, OR
// - Some combination that doesn't exceed 10 total weight
```

### Distributed Database Rate Limiting

For applications with multiple instances sharing the same database:

```go
// Use Redis for distributed coordination
rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
store, _ := gothrottle.NewRedisStore(rdb)

// All app instances using this same ID will share the rate limits
throttledDB, _ := NewDatabaseThrottler(db, gothrottle.Options{
    ID:            "shared-db-limiter",
    MaxConcurrent: 20, // Total across ALL instances
    MinTime:       5 * time.Millisecond,
    Datastore:     store,
})
```

## Real-World Use Cases & Examples

### 1. API Rate Limiting Middleware

Protect your API endpoints from being overwhelmed:

```go
package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "time"
    
    "github.com/AFZidan/gothrottle"
    "github.com/go-redis/redis/v8"
)

// APIThrottler wraps HTTP handlers with rate limiting
type APIThrottler struct {
    limiter *gothrottle.Limiter
}

func NewAPIThrottler(opts gothrottle.Options) (*APIThrottler, error) {
    limiter, err := gothrottle.NewLimiter(opts)
    if err != nil {
        return nil, err
    }
    return &APIThrottler{limiter: limiter}, nil
}

// ThrottleHandler wraps an HTTP handler with rate limiting
func (at *APIThrottler) ThrottleHandler(handler http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        _, err := at.limiter.Schedule(func() (interface{}, error) {
            handler(w, r)
            return nil, nil
        })
        
        if err != nil {
            http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
            return
        }
    }
}

func main() {
    // Create distributed rate limiter for API
    rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
    store, _ := gothrottle.NewRedisStore(rdb)
    
    throttler, _ := NewAPIThrottler(gothrottle.Options{
        ID:            "api-rate-limiter",
        MaxConcurrent: 100,    // Max 100 concurrent API requests
        MinTime:       10 * time.Millisecond, // 10ms between requests
        Datastore:     store,
    })
    
    // Apply throttling to endpoints
    http.HandleFunc("/api/users", throttler.ThrottleHandler(handleUsers))
    http.HandleFunc("/api/orders", throttler.ThrottleHandler(handleOrders))
    
    http.ListenAndServe(":8080", nil)
}

func handleUsers(w http.ResponseWriter, r *http.Request) {
    // Simulate database query
    time.Sleep(50 * time.Millisecond)
    json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

func handleOrders(w http.ResponseWriter, r *http.Request) {
    // Simulate heavy database operation
    time.Sleep(200 * time.Millisecond)
    json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}
```

### 2. File Processing Pipeline

Throttle file processing to prevent system overload:

```go
package main

import (
    "fmt"
    "io/ioutil"
    "os"
    "path/filepath"
    "time"
    
    "github.com/AFZidan/gothrottle"
)

type FileProcessor struct {
    limiter *gothrottle.Limiter
}

func NewFileProcessor() *FileProcessor {
    limiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 5,     // Process max 5 files concurrently
        MinTime:       100 * time.Millisecond, // 100ms between file processing
    })
    
    return &FileProcessor{limiter: limiter}
}

func (fp *FileProcessor) ProcessFile(filePath string) error {
    _, err := fp.limiter.ScheduleWithOptions(func() (interface{}, error) {
        // Determine file size for weight calculation
        stat, err := os.Stat(filePath)
        if err != nil {
            return nil, err
        }
        
        // Read and process file
        data, err := ioutil.ReadFile(filePath)
        if err != nil {
            return nil, err
        }
        
        // Simulate processing time based on file size
        processingTime := time.Duration(len(data)/1024) * time.Millisecond
        time.Sleep(processingTime)
        
        fmt.Printf("Processed file: %s (%d bytes)\n", filePath, len(data))
        return nil, nil
    }, 5, fp.getFileWeight(filePath)) // Priority 5, weight based on file size
    
    return err
}

func (fp *FileProcessor) getFileWeight(filePath string) int {
    stat, err := os.Stat(filePath)
    if err != nil {
        return 1
    }
    
    // Weight based on file size (MB)
    weight := int(stat.Size() / (1024 * 1024))
    if weight < 1 {
        weight = 1
    }
    if weight > 10 {
        weight = 10 // Cap at weight 10
    }
    
    return weight
}

func (fp *FileProcessor) ProcessDirectory(dirPath string) error {
    return filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        
        if !info.IsDir() {
            return fp.ProcessFile(path)
        }
        
        return nil
    })
}

func (fp *FileProcessor) Close() {
    fp.limiter.Stop()
}
```

### 3. Web Scraping with Rate Limits

Respectful web scraping that doesn't overwhelm target servers:

```go
package main

import (
    "fmt"
    "io/ioutil"
    "net/http"
    "time"
    
    "github.com/AFZidan/gothrottle"
)

type WebScraper struct {
    limiter *gothrottle.Limiter
    client  *http.Client
}

func NewWebScraper() *WebScraper {
    // Respectful scraping limits
    limiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 3,     // Max 3 concurrent requests
        MinTime:       2 * time.Second, // 2 seconds between requests
    })
    
    return &WebScraper{
        limiter: limiter,
        client:  &http.Client{Timeout: 30 * time.Second},
    }
}

func (ws *WebScraper) ScrapeURL(url string) (string, error) {
    result, err := ws.limiter.Schedule(func() (interface{}, error) {
        resp, err := ws.client.Get(url)
        if err != nil {
            return nil, err
        }
        defer resp.Body.Close()
        
        body, err := ioutil.ReadAll(resp.Body)
        if err != nil {
            return nil, err
        }
        
        fmt.Printf("Scraped: %s (%d bytes)\n", url, len(body))
        return string(body), nil
    })
    
    if err != nil {
        return "", err
    }
    
    return result.(string), nil
}

func (ws *WebScraper) ScrapeMultipleURLs(urls []string) []string {
    results := make([]string, len(urls))
    
    for i, url := range urls {
        content, err := ws.ScrapeURL(url)
        if err != nil {
            fmt.Printf("Error scraping %s: %v\n", url, err)
            continue
        }
        results[i] = content
    }
    
    return results
}

func (ws *WebScraper) Close() {
    ws.limiter.Stop()
}
```

### 4. Background Job Processing

Throttle background jobs to prevent resource exhaustion:

```go
package main

import (
    "fmt"
    "sync"
    "time"
    
    "github.com/AFZidan/gothrottle"
)

type JobType int

const (
    EmailJob JobType = iota
    ReportJob
    DataSyncJob
    ImageProcessingJob
)

type Job struct {
    ID       string
    Type     JobType
    Data     interface{}
    Priority int
}

type JobProcessor struct {
    limiter *gothrottle.Limiter
}

func NewJobProcessor() *JobProcessor {
    limiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 10,    // Max 10 concurrent jobs
        MinTime:       50 * time.Millisecond, // 50ms between job starts
    })
    
    return &JobProcessor{limiter: limiter}
}

func (jp *JobProcessor) ProcessJob(job Job) error {
    priority := job.Priority
    weight := jp.getJobWeight(job.Type)
    
    _, err := jp.limiter.ScheduleWithOptions(func() (interface{}, error) {
        return jp.executeJob(job)
    }, priority, weight)
    
    return err
}

func (jp *JobProcessor) getJobWeight(jobType JobType) int {
    switch jobType {
    case EmailJob:
        return 1 // Light operation
    case ReportJob:
        return 3 // Medium operation
    case DataSyncJob:
        return 5 // Heavy operation
    case ImageProcessingJob:
        return 8 // Very heavy operation
    default:
        return 1
    }
}

func (jp *JobProcessor) executeJob(job Job) (interface{}, error) {
    start := time.Now()
    
    switch job.Type {
    case EmailJob:
        return jp.processEmail(job)
    case ReportJob:
        return jp.generateReport(job)
    case DataSyncJob:
        return jp.syncData(job)
    case ImageProcessingJob:
        return jp.processImage(job)
    }
    
    fmt.Printf("Job %s completed in %v\n", job.ID, time.Since(start))
    return nil, nil
}

func (jp *JobProcessor) processEmail(job Job) (interface{}, error) {
    time.Sleep(100 * time.Millisecond) // Simulate email sending
    fmt.Printf("Email sent: %s\n", job.ID)
    return "email_sent", nil
}

func (jp *JobProcessor) generateReport(job Job) (interface{}, error) {
    time.Sleep(2 * time.Second) // Simulate report generation
    fmt.Printf("Report generated: %s\n", job.ID)
    return "report_generated", nil
}

func (jp *JobProcessor) syncData(job Job) (interface{}, error) {
    time.Sleep(5 * time.Second) // Simulate data sync
    fmt.Printf("Data synced: %s\n", job.ID)
    return "data_synced", nil
}

func (jp *JobProcessor) processImage(job Job) (interface{}, error) {
    time.Sleep(10 * time.Second) // Simulate image processing
    fmt.Printf("Image processed: %s\n", job.ID)
    return "image_processed", nil
}

func (jp *JobProcessor) ProcessJobsConcurrently(jobs []Job) {
    var wg sync.WaitGroup
    
    for _, job := range jobs {
        wg.Add(1)
        go func(j Job) {
            defer wg.Done()
            if err := jp.ProcessJob(j); err != nil {
                fmt.Printf("Job %s failed: %v\n", j.ID, err)
            }
        }(job)
    }
    
    wg.Wait()
}

func (jp *JobProcessor) Close() {
    jp.limiter.Stop()
}
```

### 5. Microservices Communication Throttling

Rate limit calls between microservices:

```go
package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "net/http"
    "time"
    
    "github.com/AFZidan/gothrottle"
    "github.com/go-redis/redis/v8"
)

type ServiceClient struct {
    limiter    *gothrottle.Limiter
    baseURL    string
    httpClient *http.Client
}

func NewServiceClient(serviceName, baseURL string) *ServiceClient {
    // Use Redis for distributed rate limiting across service instances
    rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
    store, _ := gothrottle.NewRedisStore(rdb)
    
    limiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        ID:            fmt.Sprintf("service-client-%s", serviceName),
        MaxConcurrent: 20,    // Max 20 concurrent calls to this service
        MinTime:       10 * time.Millisecond, // 10ms between calls
        Datastore:     store,
    })
    
    return &ServiceClient{
        limiter:    limiter,
        baseURL:    baseURL,
        httpClient: &http.Client{Timeout: 30 * time.Second},
    }
}

func (sc *ServiceClient) Get(endpoint string) (*http.Response, error) {
    result, err := sc.limiter.ScheduleWithOptions(func() (interface{}, error) {
        url := sc.baseURL + endpoint
        return sc.httpClient.Get(url)
    }, 5, 1) // Normal priority, weight 1
    
    if err != nil {
        return nil, err
    }
    
    return result.(*http.Response), nil
}

func (sc *ServiceClient) Post(endpoint string, data interface{}) (*http.Response, error) {
    result, err := sc.limiter.ScheduleWithOptions(func() (interface{}, error) {
        jsonData, err := json.Marshal(data)
        if err != nil {
            return nil, err
        }
        
        url := sc.baseURL + endpoint
        return sc.httpClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
    }, 6, 2) // Higher priority, weight 2 (POST is heavier)
    
    if err != nil {
        return nil, err
    }
    
    return result.(*http.Response), nil
}

func (sc *ServiceClient) BulkOperation(endpoint string, items []interface{}) error {
    _, err := sc.limiter.ScheduleWithOptions(func() (interface{}, error) {
        // Bulk operations are heavy and should have high priority
        jsonData, err := json.Marshal(items)
        if err != nil {
            return nil, err
        }
        
        url := sc.baseURL + endpoint
        resp, err := sc.httpClient.Post(url, "application/json", bytes.NewBuffer(jsonData))
        if err != nil {
            return nil, err
        }
        defer resp.Body.Close()
        
        return resp, nil
    }, 10, 5) // Highest priority, weight 5 (very heavy operation)
    
    return err
}

func (sc *ServiceClient) Close() {
    sc.limiter.Stop()
}

// Example usage in a microservice
func main() {
    userService := NewServiceClient("user-service", "http://user-service:8080")
    orderService := NewServiceClient("order-service", "http://order-service:8080")
    
    defer userService.Close()
    defer orderService.Close()
    
    // These calls will be rate limited
    userResp, _ := userService.Get("/api/users/123")
    orderResp, _ := orderService.Post("/api/orders", map[string]interface{}{
        "user_id": 123,
        "amount":  99.99,
    })
    
    fmt.Printf("User response status: %d\n", userResp.StatusCode)
    fmt.Printf("Order response status: %d\n", orderResp.StatusCode)
}
```

### 6. ETL Pipeline Rate Limiting

Control data extraction, transformation, and loading processes:

```go
package main

import (
    "database/sql"
    "fmt"
    "time"
    
    "github.com/AFZidan/gothrottle"
    _ "github.com/lib/pq"
)

type ETLPipeline struct {
    extractLimiter   *gothrottle.Limiter
    transformLimiter *gothrottle.Limiter
    loadLimiter      *gothrottle.Limiter
    sourceDB         *sql.DB
    targetDB         *sql.DB
}

func NewETLPipeline(sourceDB, targetDB *sql.DB) *ETLPipeline {
    // Different rate limits for different stages
    extractLimiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 5,  // Limit source DB queries
        MinTime:       20 * time.Millisecond,
    })
    
    transformLimiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 10, // CPU-intensive, but can be parallel
        MinTime:       10 * time.Millisecond,
    })
    
    loadLimiter, _ := gothrottle.NewLimiter(gothrottle.Options{
        MaxConcurrent: 3,  // Limit target DB writes
        MinTime:       50 * time.Millisecond,
    })
    
    return &ETLPipeline{
        extractLimiter:   extractLimiter,
        transformLimiter: transformLimiter,
        loadLimiter:      loadLimiter,
        sourceDB:         sourceDB,
        targetDB:         targetDB,
    }
}

func (etl *ETLPipeline) ExtractData(query string) ([]map[string]interface{}, error) {
    result, err := etl.extractLimiter.Schedule(func() (interface{}, error) {
        rows, err := etl.sourceDB.Query(query)
        if err != nil {
            return nil, err
        }
        defer rows.Close()
        
        // Process rows into data structure
        var data []map[string]interface{}
        // ... row processing logic
        
        fmt.Printf("Extracted %d records\n", len(data))
        return data, nil
    })
    
    if err != nil {
        return nil, err
    }
    
    return result.([]map[string]interface{}), nil
}

func (etl *ETLPipeline) TransformData(data []map[string]interface{}) ([]map[string]interface{}, error) {
    result, err := etl.transformLimiter.Schedule(func() (interface{}, error) {
        // Simulate data transformation
        time.Sleep(100 * time.Millisecond)
        
        var transformed []map[string]interface{}
        for _, record := range data {
            // Transform each record
            transformedRecord := make(map[string]interface{})
            for k, v := range record {
                transformedRecord[k+"_transformed"] = v
            }
            transformed = append(transformed, transformedRecord)
        }
        
        fmt.Printf("Transformed %d records\n", len(transformed))
        return transformed, nil
    })
    
    if err != nil {
        return nil, err
    }
    
    return result.([]map[string]interface{}), nil
}

func (etl *ETLPipeline) LoadData(data []map[string]interface{}) error {
    _, err := etl.loadLimiter.Schedule(func() (interface{}, error) {
        tx, err := etl.targetDB.Begin()
        if err != nil {
            return nil, err
        }
        defer tx.Rollback()
        
        for _, record := range data {
            // Insert transformed record
            // ... insert logic
        }
        
        err = tx.Commit()
        if err != nil {
            return nil, err
        }
        
        fmt.Printf("Loaded %d records\n", len(data))
        return nil, nil
    })
    
    return err
}

func (etl *ETLPipeline) ProcessBatch(query string) error {
    // Extract -> Transform -> Load pipeline
    data, err := etl.ExtractData(query)
    if err != nil {
        return err
    }
    
    transformedData, err := etl.TransformData(data)
    if err != nil {
        return err
    }
    
    return etl.LoadData(transformedData)
}

func (etl *ETLPipeline) Close() {
    etl.extractLimiter.Stop()
    etl.transformLimiter.Stop()
    etl.loadLimiter.Stop()
}
```
