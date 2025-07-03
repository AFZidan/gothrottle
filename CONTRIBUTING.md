# Contributing to GoThrottle

Thank you for your interest in contributing to GoThrottle! We welcome contributions from everyone.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [How to Contribute](#how-to-contribute)
- [Pull Request Process](#pull-request-process)
- [Coding Standards](#coding-standards)
- [Testing](#testing)
- [Documentation](#documentation)

## Code of Conduct

By participating in this project, you agree to abide by our Code of Conduct. Please be respectful and constructive in all interactions.

## Getting Started

1. Fork the repository on GitHub
2. Clone your fork locally
3. Create a new branch for your changes
4. Make your changes and commit them
5. Push to your fork and submit a pull request

## Development Setup

### Prerequisites

- Go 1.19 or later
- Redis (for testing RedisStore functionality)
- Git

### Setup Steps

```bash
# Clone your fork
git clone https://github.com/YOUR_USERNAME/gothrottle.git
cd gothrottle

# Install dependencies
go mod download

# Run tests to ensure everything works
go test ./tests/... -v

# Start Redis for integration tests (if needed)
redis-server --daemonize yes
```

## How to Contribute

### Reporting Bugs

1. Check if the bug has already been reported in [Issues](https://github.com/AFZidan/gothrottle/issues)
2. If not, create a new issue using the Bug Report template
3. Provide as much detail as possible, including:
   - Go version
   - Operating system
   - Steps to reproduce
   - Expected vs actual behavior
   - Code examples

### Suggesting Features

1. Check if the feature has already been suggested in [Issues](https://github.com/AFZidan/gothrottle/issues)
2. If not, create a new issue using the Feature Request template
3. Describe:
   - The problem you're trying to solve
   - Your proposed solution
   - Example usage
   - Any alternatives you considered

### Contributing Code

1. Look for issues labeled `good first issue` or `help wanted`
2. Comment on the issue to let others know you're working on it
3. Fork the repository and create a feature branch
4. Make your changes following our coding standards
5. Add tests for your changes
6. Ensure all tests pass
7. Update documentation if needed
8. Submit a pull request

## Pull Request Process

1. **Create a clear title**: Use a descriptive title that explains what your PR does
2. **Fill out the template**: Use the provided PR template
3. **Reference issues**: Link to any related issues
4. **Small, focused changes**: Keep PRs focused on a single concern
5. **Tests required**: All new functionality must include tests
6. **Documentation**: Update relevant documentation
7. **CI must pass**: All GitHub Actions checks must pass

### PR Requirements

- [ ] Code follows project coding standards
- [ ] Tests are included and pass
- [ ] Documentation is updated
- [ ] CI/CD pipeline passes
- [ ] No breaking changes (unless discussed)

## Coding Standards

### Go Code Style

- Follow the [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- Use `go fmt` to format your code
- Use `go vet` to check for common errors
- Use `golangci-lint` for additional linting

### Naming Conventions

- Use descriptive names for variables, functions, and types
- Follow Go naming conventions (e.g., `camelCase` for unexported, `PascalCase` for exported)
- Use meaningful commit messages

### Code Organization

- Keep functions small and focused
- Add comments for complex logic
- Use interfaces to define contracts
- Separate concerns appropriately

### Example Code Style

```go
// Good: Clear function name and documentation
// ProcessJob schedules a job with the specified priority and weight.
// It returns an error if the job cannot be scheduled.
func (l *Limiter) ProcessJob(task func() (interface{}, error), priority, weight int) error {
    if weight <= 0 {
        return ErrInvalidWeight
    }
    
    // Implementation here
    return nil
}

// Bad: Unclear naming and no documentation
func (l *Limiter) pj(t func() (interface{}, error), p, w int) error {
    // Implementation without proper validation
    return nil
}
```

## Testing

### Test Requirements

- All new code must include tests
- Tests should cover both success and error cases
- Use table-driven tests for multiple scenarios
- Include benchmarks for performance-critical code

### Running Tests

```bash
# Run all tests
go test ./tests/... -v

# Run tests with coverage
go test -coverprofile=coverage.out ./tests/...
go tool cover -html=coverage.out

# Run benchmarks
go test -bench=. ./tests/...

# Run tests with race detection
go test -race ./tests/...
```

### Test Structure

```go
func TestLimiter_NewFeature(t *testing.T) {
    tests := []struct {
        name        string
        input       InputType
        expected    ExpectedType
        expectError bool
    }{
        {
            name:        "valid input",
            input:       validInput,
            expected:    expectedOutput,
            expectError: false,
        },
        // More test cases...
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Test implementation
        })
    }
}
```

## Documentation

### What to Document

- All public functions and types
- Complex algorithms or logic
- Usage examples
- Configuration options
- Error conditions

### Documentation Style

- Use clear, concise language
- Include code examples
- Update README.md for new features
- Add godoc comments for all public APIs

### Example Documentation

```go
// NewLimiter creates a new rate limiter with the specified options.
// 
// The limiter will use LocalStore by default if no Datastore is specified.
// For distributed rate limiting, provide a RedisStore in the options.
//
// Example:
//   limiter, err := gothrottle.NewLimiter(gothrottle.Options{
//       MaxConcurrent: 10,
//       MinTime:       100 * time.Millisecond,
//   })
//
// Returns an error if the options are invalid or if the datastore
// cannot be initialized.
func NewLimiter(opts Options) (*Limiter, error) {
    // Implementation
}
```

## Release Process

1. Update version in relevant files
2. Update CHANGELOG.md
3. Create a new tag: `git tag v1.x.x`
4. Push the tag: `git push origin v1.x.x`
5. GitHub Actions will automatically create a release

## Questions?

If you have questions about contributing, please:

1. Check the existing documentation
2. Search through existing issues
3. Create a new issue with your question
4. Join our community discussions

Thank you for contributing to GoThrottle! 🚀
