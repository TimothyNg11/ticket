# 2. Bootstrap admins with ADMIN_EMAILS

**Status:** accepted (Phase 1)

## Context

Admin endpoints require an admin, but the API needs some way to create the first one.

## Decision

The `ADMIN_EMAILS` setting lists emails (comma separated, case-insensitive) that receive the `admin` role when they register. Everyone else gets `user`.

## Consequences

- Zero extra tooling: set the variable, register, done. Works the same in Compose, CI, and Kubernetes.
- Whoever registers a listed email first becomes an admin. In production, list only addresses you control, and register them before opening the API, or clear the setting after the first deploy.
- Role changes after registration need SQL for now.
- Alternative considered: an `api create-admin` subcommand. It's safer in production but adds a command and a manual step to every environment. It's easy to add later if needed.
