# gitops

Deployment state, not code. Each file in `environments/` pins what an
environment runs; Argo CD (see `deploy/argocd/application.yaml` on `main`)
applies the Helm chart from `main` with these values.

CI updates `image.tag` after every merge to `main`. Every deploy and rollback
is therefore a commit here, with history and authorship.
