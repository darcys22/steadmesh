#!/usr/bin/env bash
# Points quickstart/ at a release: module ?ref= tags, the platform version and
# the provider constraint. Usage: hack/set-version.sh 0.2.0
# With --check, changes nothing and fails unless quickstart/ already pins it.
set -euo pipefail
check=false
if [[ "${1:-}" == "--check" ]]; then check=true; shift; fi
v="${1:?usage: $0 [--check] <version without the v>}"
[[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "not a semantic version: $v" >&2; exit 2; }
root="$(cd "$(dirname "$0")/.." && pwd)"

pin() { # directory
  find "$1" -name '*.tf' -exec perl -0pi -e "
    s#\?ref=v[0-9.]+#?ref=v$v#g;
    s#(steadmesh_version\s+= )\"[0-9.]+\"#\$1\"$v\"#g;
    s#(source\s+= \"darcys22/steadmesh\"\n\s+version\s+= )\"[^\"]*\"#\$1\"~> $v\"#g;
  " {} +
}

if $check; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  cp -R "$root/quickstart" "$tmp/quickstart"
  pin "$tmp/quickstart"
  if ! diff -ru "$root/quickstart" "$tmp/quickstart" >&2; then
    echo "quickstart/ does not pin $v; run hack/set-version.sh $v" >&2
    exit 1
  fi
  echo "quickstart/ pins $v"
else
  pin "$root/quickstart"
fi
