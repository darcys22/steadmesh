#!/usr/bin/env bash
# Build steadmesh images with a filtered context (no bin/, examples/, modules/).
# Usage: build/build.sh <image>... [-- extra docker build args]
#   images: controller platform fakes seat-fake seat-claudecode
#   TAG (default dev) and REGISTRY (default steadmesh) set the image name.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
tag="${TAG:-dev}"
registry="${REGISTRY:-steadmesh}"
images=()
while [[ $# -gt 0 && "$1" != "--" ]]; do images+=("$1"); shift; done
[[ "${1:-}" == "--" ]] && shift
[[ ${#images[@]} -gt 0 ]] || { echo "usage: $0 <image>... [-- docker args]" >&2; exit 2; }
excludes=()
while IFS= read -r line; do
  [[ -z "$line" || "$line" == \#* ]] && continue
  excludes+=("--exclude=./${line#\*\*/}")
done < "$root/build/dockerignore"
tarflags=()
if tar --version 2>/dev/null | grep -q bsdtar; then tarflags=(--no-xattrs --no-mac-metadata --no-acls); fi
for img in "${images[@]}"; do
  df="build/$img.Dockerfile"
  [[ -f "$root/$df" ]] || { echo "no $df" >&2; exit 2; }
  echo "==> $registry/$img:$tag"
  # Keep build/ in the context: the Dockerfile is read from the tar stream.
  # Strip macOS xattrs/metadata, which the daemon rejects (lsetxattr).
  COPYFILE_DISABLE=1 tar -C "$root" -c "${tarflags[@]}" "${excludes[@]}" . | docker build -f "$df" -t "$registry/$img:$tag" "$@" -
done
