# 8. Kubernetes deployment choices

**Status:** accepted (Phase 7)

## Ingress controller: Traefik, not ingress-nginx

The spec named the NGINX ingress controller. The `kubernetes/ingress-nginx` project was retired and its repository archived in March 2026, so it no longer gets security fixes. The chart uses the standard `networking.k8s.io/v1` Ingress with a configurable `ingressClassName`, and local clusters run Traefik. Moving to Gateway API later only touches the ingress template.

## Migrations: API init container, not a Helm hook Job

A pre-install hook Job can't work when the chart also installs Postgres, because hooks run before the database exists. Instead, each API pod runs `api migrate` in an init container. golang-migrate's advisory lock makes concurrent pods safe, and no pod serves before its schema is current.

Consequence: every migration must be backward compatible with the release before it (expand/contract), because old and new pods overlap during a rolling update.

## Postgres connections through PgBouncer

App traffic goes through PgBouncer in transaction mode; migrations connect directly, because their session-level advisory lock doesn't survive transaction pooling. pgx's prepared statements rely on PgBouncer 1.21+ `max_prepared_statements`.

## In-chart Postgres and Redis for local clusters only

The spec suggests the CloudNativePG operator. For a local cluster, a plain StatefulSet is enough and has no extra operator to install. `values-cloud.yaml` disables both in favor of managed services; that's where high availability belongs.

## Hash concurrency and GOMEMLIMIT

The OOM described in LEARNING.md led to two permanent settings: Argon2id hashing is capped per pod (`ARGON2_CONCURRENCY`, default 4), and `GOMEMLIMIT` is derived from the container memory limit.
