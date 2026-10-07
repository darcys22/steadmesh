#!/usr/bin/env bash
# Points quickstart/ and the provider documentation sources (provider/docs-examples,
# provider/docs-templates) at a release: module ?ref= tags, the platform version
# and the provider constraint. Usage: hack/set-version.sh 0.2.0, then
# `make provider-docs` to re-render provider/docs.
# With --check, changes nothing and fails unless they already pin it.
set -euo pipefail
check=false
if [[ "${1:-}" == "--check" ]]; then check=true; shift; fi
v="${1:?usage: $0 [--check] <version without the v>}"
[[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "not a semantic version: $v" >&2; exit 2; }
root="$(cd "$(dirname "$0")/.." && pwd)"

pin() { # directory
  find "$1" \( -name '*.tf' -o -name '*.tmpl' \) -exec perl -0pi -e "
    s#\?ref=v[0-9.]+#?ref=v$v#g;
    s#(ghcr\.io/darcys22/steadmesh/[a-z-]+):[0-9.]+#\$1:$v#g;
    s#(steadmesh_version\s+= )\"[0-9.]+\"#\$1\"$v\"#g;
    s#(source\s+= \"darcys22/steadmesh\"\n\s+version\s+= )\"[^\"]*\"#\$1\"~> $v\"#g;
  " {} +
}

dirs=(quickstart provider/docs-examples provider/docs-templates)

if $check; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  for d in "${dirs[@]}"; do
    mkdir -p "$tmp/$(dirname "$d")"
    cp -R "$root/$d" "$tmp/$d"
    pin "$tmp/$d"
    if ! diff -ru "$root/$d" "$tmp/$d" >&2; then
      echo "$d/ does not pin $v; run hack/set-version.sh $v" >&2
      exit 1
    fi
  done
  echo "${dirs[*]} pin $v"
else
  for d in "${dirs[@]}"; do pin "$root/$d"; done
fi
