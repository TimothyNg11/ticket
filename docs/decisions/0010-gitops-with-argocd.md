# 10. GitOps with Argo CD, app of apps, a separate gitops branch

**Status:** accepted (Phase 10)

## Context

Merging to `main` should update the running cluster with no manual steps, every deploy should be auditable, and rolling back should be as simple as undoing a change.

## Decision

- **Argo CD** watches git and keeps the cluster in sync (`automated`, `prune`, `selfHeal`), instead of CI pushing to the cluster with `kubectl` or `helm`. The cluster pulls; CI never needs cluster credentials.
- **Deployment state on a `gitops` branch.** CI can't push to `main` (pull request and passing checks required), and code history shouldn't be mixed with deploy history. After every merge whose checks all pass (including the kind end-to-end test), CI commits the merge SHA as the image tag in `apps/ticket.yaml`.
- **App of apps.** Argo CD can't read one repository at two revisions (`main` for the chart, `gitops` for the tag) within one Application. A root Application watches `apps/` on `gitops`; `apps/ticket.yaml` is an Application that renders the chart from `main` with the tag inline.
- **Images are public on GHCR**, so the cluster pulls without credentials. A private registry would add an image pull secret.

## Consequences

- Deploy = commit on `gitops`; rollback = `git revert` of that commit. Both are visible in history.
- CI runs finish in any order, so promotion only moves the pin forward (`.github/scripts/promote.sh` skips a build the pinned one already contains). A deliberate rollback is a manual commit on `gitops`.
- The chart is rendered from `main` while the image is pinned, so a merge's template changes reach the cluster before its image does. Chart changes must work with the previous image.
- A merge reaches the cluster in roughly CI time plus Argo CD's 60 s poll. A GitHub webhook to Argo CD would remove the poll delay.
- Rollbacks cross migrations, so migrations must stay backward compatible, and the migrator tolerates a schema newer than the build (`TestMigrateToleratesNewerSchema`).
- Canary releases (Argo Rollouts) and a cloud deployment were optional in the spec and are not done; `values-cloud.yaml` covers the configuration side.
