#!/usr/bin/env bash
# Pins image tag $1 (a commit on main) in apps/ticket.yaml on the gitops branch.
# Run from a checkout of gitops that also has main's history. CI runs finish in
# any order, so a build only moves the pin forward: if the pinned commit already
# contains this one, a newer build is deployed and there is nothing to do.
set -euo pipefail
sha=$1
for attempt in 1 2 3; do
  git fetch -q origin gitops
  git reset -q --hard origin/gitops
  pinned=$(sed -nE 's/^ *tag: *//p' apps/ticket.yaml)
  if git merge-base --is-ancestor "$sha" "$pinned" 2>/dev/null; then
    echo "::notice::${pinned::7} is already deployed and includes ${sha::7}; nothing to do"
    exit 0
  fi
  sed -i -E "s/^( *)tag: .*/\1tag: ${sha}/" apps/ticket.yaml
  git commit -qam "Deploy ${sha::7} to kind" -m "Built and tested from main at ${sha}."
  # Lost a race with another promotion: start again from the new pin.
  git push -q origin HEAD:gitops && exit 0
done
exit 1
