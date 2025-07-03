# Security Policy

## Supported Versions

We currently support the following versions of GoThrottle with security updates:

| Version | Supported          |
| ------- | ------------------ |
| 1.0.x   | :white_check_mark: |

## Reporting a Vulnerability

The GoThrottle team takes security bugs seriously. We appreciate your efforts to responsibly disclose your findings, and will make every effort to acknowledge your contributions.

To report a security issue, please use the GitHub Security Advisory ["Report a Vulnerability"](https://github.com/AFZidan/gothrottle/security/advisories/new) tab.

The GoThrottle team will send a response indicating the next steps in handling your report. After the initial reply to your report, the security team will keep you informed of the progress towards a fix and full announcement, and may ask for additional information or guidance.

## Security Best Practices

When using GoThrottle in production:

1. **Redis Security**: If using RedisStore, ensure your Redis instance is properly secured:
   - Use authentication (`requirepass`)
   - Configure firewall rules to restrict access
   - Use TLS encryption for Redis connections
   - Keep Redis updated to the latest stable version

2. **Rate Limiting**: Configure appropriate limits for your use case:
   - Set reasonable `MaxConcurrent` values to prevent resource exhaustion
   - Use appropriate `MinTime` values to prevent overwhelming downstream services
   - Monitor your application for unexpected behavior

3. **Error Handling**: Always handle errors returned by GoThrottle methods:
   - Check for errors from `NewLimiter()`, `Schedule()`, and `ScheduleWithOptions()`
   - Implement appropriate fallback mechanisms
   - Log errors for monitoring and debugging

4. **Resource Management**: Ensure proper cleanup:
   - Always call `limiter.Stop()` when shutting down your application
   - Use `defer limiter.Stop()` to ensure cleanup even in case of panics
   - Monitor for goroutine leaks in long-running applications

## Reporting Process

1. Report security vulnerabilities through GitHub's private vulnerability reporting feature
2. Do not create public issues for security vulnerabilities
3. Allow up to 48 hours for an initial response
4. Work with the maintainers to understand and address the issue
5. A public disclosure will be made after a fix is available

## Recognition

We will acknowledge security researchers who report vulnerabilities to us in our release notes and security advisories, unless they prefer to remain anonymous.
