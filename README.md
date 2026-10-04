# gitops

Deployment state, not code. `apps/` holds the Argo CD Application for each
environment, including the exact image tag (commit SHA) it runs. The root app
(`deploy/argocd/root.yaml` on `main`) applies whatever is here.

CI updates the tag after every merge to `main` that passes all checks, so every
deploy and every rollback is a commit on this branch, with history and an author.
