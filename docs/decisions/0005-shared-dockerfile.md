# 5. One Dockerfile for all Go services

**Status:** accepted (Phase 2)

## Context

The spec suggests one Dockerfile per service. With three Go binaries (`api`, `workers`, `payments_mock`) in one module, those files would be identical except for the package path.

## Decision

A single root `Dockerfile` takes `--build-arg SERVICE=<dir under services/>`. All images share the same build stage, the distroless non-root runtime, and dependency caching.

## Consequences

- One place to update the Go version, base image, or build flags.
- Health checks live in Compose and Kubernetes instead of the Dockerfile, since only the API has a `healthcheck` subcommand.
- A service that needed different system packages would need its own Dockerfile; none does.
