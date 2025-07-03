# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Initial release of GoThrottle
- Local rate limiting with LocalStore
- Distributed rate limiting with RedisStore
- Priority queue for job scheduling
- Weight-based job scheduling
- Comprehensive test suite
- Database throttling examples
- Real-world usage examples
- GitHub Actions CI/CD pipeline
- Issue and PR templates
- Contributing guidelines

### Features

- **Local and Distributed Rate Limiting**: Support for both in-memory and Redis-based backends
- **Configurable Limits**: Maximum concurrent jobs and minimum time between jobs
- **Priority Queue**: Jobs executed based on priority (higher priority first)
- **Job Weights**: Different resource costs for different operations
- **Atomic Operations**: Redis operations use Lua scripts for race-condition-free coordination
- **Easy Integration**: Simple API for wrapping existing functions
- **Comprehensive Testing**: Unit tests, integration tests, and benchmarks
- **Production Ready**: Proper error handling, graceful shutdown, and resource cleanup

### Documentation

- Complete API reference
- Quick start guide
- Architecture overview
- Real-world usage examples (API middleware, file processing, web scraping, etc.)
- Database throttling patterns
- Contribution guidelines

## [1.0.0] - 2025-07-03

### Initial Release

- Initial public release
