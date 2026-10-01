# 3. OpenAPI first, with oapi-codegen's strict server

**Status:** accepted (Phase 1)

## Context

The API needs a written contract that clients and contract tests can rely on, and handlers that can't drift from it.

## Decision

`api/openapi.yaml` is the source of truth. oapi-codegen generates models, chi routing, and a *strict* server interface: each handler receives a typed request object and returns a typed response or an error. A compile-time assertion makes the build fail if any operation lacks a handler. Validation rules live in the spec as `x-oapi-codegen-extra-tags` and are enforced with go-playground/validator.

## Consequences

- Request decoding, path-parameter parsing, and response encoding are generated, so handlers stay short.
- Every error flows through one function, which guarantees the single error shape.
- Nested inline schemas generate awkward anonymous structs, so nested objects are given names in the spec (for example `VenueSection` and `SectionPrice`).
- Generated code is committed so builds don't need the generator; CI (Phase 3) checks that it is up to date.
