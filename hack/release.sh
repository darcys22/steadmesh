#!/usr/bin/env bash
# Cuts a release: bumps the version from the latest vX.Y.Z tag, points the
# quickstart and the provider docs at it, re-renders provider/docs, commits
# "Release vX.Y.Z", tags it and pushes main and the tag. The tag starts
# .github/workflows/release.yml, which publishes the images, chart, provider
# and GitHub release (README.md, "Releasing").
#
#   hack/release.sh              bump the minor version (0.1.1 -> 0.2.0)
#   BUMP=patch hack/release.sh   or patch / major
#   VERSION=1.0.0 hack/release.sh
#   YES=1 hack/release.sh        push without asking
#   DRY_RUN=1 hack/release.sh    show what would change, then undo it
#
# It refuses to run off main, with uncommitted changes, or behind origin/main,
# and runs `make lint test` before changing anything.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

die() { echo "release: $*" >&2; exit 1; }

[[ "$(git rev-parse --abbrev-ref HEAD)" == main ]] || die "not on main"
[[ -n "${DRY_RUN:-}" || -z "$(git status --porcelain)" ]] || die "uncommitted changes; commit or stash them first"
# A dry run reverts what it pins afterwards, so those paths must be clean.
[[ -z "$(git status --porcelain -- quickstart provider docs README.md tests/e2e/demo_test.go)" ]] || die "uncommitted changes in quickstart/, provider/, docs/, README.md or tests/e2e/demo_test.go"
git fetch --quiet --tags origin main
[[ -z "$(git rev-list HEAD..origin/main)" ]] || die "main is behind origin/main; pull first"

last="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -1)"
[[ -n "$last" ]] || last="v0.0.0"
IFS=. read -r major minor patch <<<"${last#v}"
if [[ -n "${VERSION:-}" ]]; then
  next="${VERSION#v}"
else
  case "${BUMP:-minor}" in
    major) next="$((major + 1)).0.0" ;;
    minor) next="$major.$((minor + 1)).0" ;;
    patch) next="$major.$minor.$((patch + 1))" ;;
    *) die "BUMP must be major, minor or patch" ;;
  esac
fi
[[ "$next" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "not a semantic version: $next"
tag="v$next"
git rev-parse -q --verify "refs/tags/$tag" >/dev/null && die "$tag already exists"
if git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null 2>&1; then die "$tag already exists on origin"; fi

echo "==> releasing $tag (previous $last)"
echo "==> make lint test"
make lint test

echo "==> pinning quickstart/, the provider docs and the website to $next"
hack/set-version.sh "$next"
make provider-docs
hack/site.py sync
hack/set-version.sh --check "$next"

if [[ "${DRY_RUN:-}" == 1 ]]; then
  echo "==> dry run: $tag would change"
  git --no-pager diff --stat -- quickstart provider docs README.md tests/e2e/demo_test.go
  git checkout -- quickstart provider docs README.md tests/e2e/demo_test.go
  echo "==> dry run: changes undone; nothing committed, tagged or pushed"
  exit 0
fi

git add -A quickstart provider docs README.md tests/e2e/demo_test.go
if git diff --cached --quiet; then
  die "nothing changed for $tag"
fi
git commit -q -m "Release $tag"
git tag -a "$tag" -m "Release $tag"
echo "==> committed and tagged $tag:"
git --no-pager log --oneline -1
git --no-pager diff --stat HEAD~1 HEAD | tail -1

if [[ "${YES:-}" != 1 ]]; then
  read -r -p "Push main and $tag to origin? This publishes the release. [y/N] " answer
  if [[ "$answer" != [yY] && "$answer" != [yY][eE][sS] ]]; then
    echo "Not pushed. To undo locally: git tag -d $tag && git reset --hard HEAD~1"
    echo "To push later: git push origin main $tag"
    exit 0
  fi
fi
git push origin main "$tag"
echo "==> pushed. The release workflow is running: https://github.com/darcys22/steadmesh/actions"
