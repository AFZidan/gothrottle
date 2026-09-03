# PR #4: v1.1.1-hardening - Audit and finalize release process

This PR addresses all seven items from the audit remediation phases, adds regression test coverage, and implements a secure, gated release workflow without merging, tagging, publishing, or modifying v1.1.0.

## Summary of Changes

### 1. Publish-time tag/release re-resolution and SHA equality
- **Problem**: Original workflow trusted the verify job's boolean for existing releases, creating a race condition
- **Fix**: Publish job now re-inspects tag and release state at publish time using `resolve-remote-tag.sh`
- **Files**: 
  - `.github/scripts/resolve-remote-tag.sh`: Resolves lightweight and annotated tags
  - `.github/workflows/release.yml`: Updated publish job to re-verify state

### 2. Fail-closed CI gate script with explicit required list
- **Problem**: Previous workflow assumed existing checks were sufficient
- **Fix**: Explicitly name 14 required checks; fail if any are missing or unsuccessful
- **Files**:
  - `.github/scripts/check-required-checks.sh`: Fail-closed validation
  - `.github/scripts/classify-checks.sh`: Defines required check list (testable without API)
  - `.github/workflows/release.yml`: Uses the check script in verify job

### 3. Go proxy verification via go mod download, no lowercasing
- **Problem**: Used lowercased URL guessing instead of actual toolchain
- **Fix**: Uses `go mod download` to get correct module path; no lowercasing
- **Files**:
  - `.github/workflows/release.yml`: Verify job uses `go list -m` and `go mod download`

### 4. Canonical Go semver validation script
- **Problem**: Standard semver validation too permissive for Go modules
- **Fix**: Stricter validation matching Go's requirements (no empty pre-release/build IDs)
- **Files**:
  - `.github/scripts/check-version.sh`: Canonical validation with comprehensive test cases
  - `.github/scripts/test-release-gates.sh`: Tests for version validation

### 5. Bounded Redis reconciliation (ZSCAN/HSCAN, ≤256 per command)
- **Problem**: Unbounded ZRANGE 0 -1 and HKEYS could exceed Lua argument limits
- **Fix**: Cursor-based ZSCAN/HSCAN iteration with ≤256 batch sizes
- **Files**:
  - `redis_lease.go`: Completely rewritten `redisReconcileHelper` function
  - `tests/redis_orphan_test.go`: Regression tests for bounded orphan reconciliation

### 6. Centralize duration conversion, ceil sub-µs, range-check every Lua value, subtraction form
- **Problem**: Inconsistent conversion, truncation of sub-µs values, potential overflow
- **Fix**: 
  - Centralized `durationMicros()` with ceiling (round up sub-µs to 1µs)
  - Range checking for all Lua-bound values (`luaMicros`, `luaMillis`, `luaInt`)
  - Subtraction-form capacity comparisons (`max_concurrent - running`)
- **Files**:
  - `bounds.go`: Central numeric policy
  - `lease.go`: Uses `durationMicros` for lease config
  - `options.go`: Validation uses centralized converters
  - `redis_store.go` & `redis_lease.go`: Updated to use centralized converters
  - `tests/store_validation_test.go`: Regression tests for direct-store validation
  - `tests/bestfit_test.go`: Regression tests for distributed SchedBestFit

### 7. Rulesets via gh API + document exact required check names
- **Problem**: No clear definition of required checks
- **Fix**: Explicitly document and enforce 14 required check names
- **Files**:
  - `.github/scripts/classify-checks.sh`: Lists exact required check names
  - `.github/workflows/release.yml`: Documents the requirement in comments

### Additional Improvements
- **Regression tests**: Added comprehensive test coverage for each fix
- **Documentation**: Updated README.md, CHANGELOG.md, AGENTS.md
- **Workflow security**: Least privilege throughout; only publish job has contents: write

### Key Technical Details
- **Numeric safety**: All values crossing into Lua go through range-checked helpers
- **Duration handling**: Sub-microsecond values round up to 1µs (safe direction)
- **Capacity comparisons**: Written as subtraction to prevent overflow
- **Redis scripts**: Use `TIME` from Redis, not client clock; TTLs only extended
- **Release workflow**: 
  - workflow_dispatch only (no tag triggers)
  - Verification before tag creation
  - Publish-time re-verification of tag/release state
  - Go proxy warm-up after publication
  - Concurrency protection to prevent overlapping runs

All changes are backward compatible except for the stricter validation which rejects previously accepted invalid values (negative limits, out-of-range values) - these now properly return errors instead of exhibiting silent incorrect behavior.