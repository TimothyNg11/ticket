#!/usr/bin/env bash
# Tests promote.sh against a throwaway origin with main and gitops branches.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail() { echo "FAIL: $*"; exit 1; }
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com

git init -q --bare "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/src" 2>/dev/null
cd "$tmp/src"
git checkout -q -b main
git commit -q --allow-empty -m one && one=$(git rev-parse HEAD)
git commit -q --allow-empty -m two && two=$(git rev-parse HEAD)
git commit -q --allow-empty -m three && three=$(git rev-parse HEAD)
git push -q origin main
git checkout -q --orphan gitops
git rm -rq --cached . 2>/dev/null || true
mkdir apps
printf 'spec:\n  values:\n    image:\n      tag: %s\n' "$one" >apps/ticket.yaml
git add apps && git commit -q -m pin && git push -q origin gitops

pinned() { git -C "$tmp/origin.git" show gitops:apps/ticket.yaml | sed -nE 's/^ *tag: *//p'; }
promote() { git clone -q "$tmp/origin.git" "$tmp/w$1" && (cd "$tmp/w$1" && git checkout -q gitops && "$here/promote.sh" "$1" >/dev/null); rm -rf "$tmp/w$1"; }

promote "$three"
[ "$(pinned)" = "$three" ] || fail "newer build was not deployed"
promote "$two"
[ "$(pinned)" = "$three" ] || fail "older build replaced a newer one"
promote "$three"
[ "$(git -C "$tmp/origin.git" rev-list --count gitops)" = 2 ] || fail "re-promoting the pinned build made a commit"
grep -q "^      tag: $three$" <(git -C "$tmp/origin.git" show gitops:apps/ticket.yaml) || fail "indentation changed"
echo "promote_test: ok"
