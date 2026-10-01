# 1. One Go module for all services

**Status:** accepted (Phase 1)

## Context

The system has several deployables: the API, background workers, and a mock payment service. Each could be its own Go module with its own `go.mod`, or they could share one.

## Decision

One module (`ticket`) at the repo root. Binaries live under `services/*`, and shared code under `internal/*`, which Go forbids importing from outside this module.

## Consequences

- Shared packages (`db`, `auth`, `apperr`) are imported directly, with no versioning or `replace` directives.
- One `go.sum` and one dependency set to scan and update.
- Services can't pin different versions of a dependency or be released independently. That doesn't matter for one repo with one owner; it would if separate teams owned services.
