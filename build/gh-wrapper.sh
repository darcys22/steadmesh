#!/bin/sh
# gh with the seat's GitHub credential: when an access profile delivers it to
# the sandbox, steadmesh-tools fetches a fresh token for this invocation only
# (docs/sandbox.html). Without such access, gh runs unauthenticated.
if [ -z "${GH_TOKEN:-}" ] && [ -z "${GITHUB_TOKEN:-}" ]; then
  if t="$(/usr/local/bin/steadmesh-tools credential token 2>/dev/null)"; then
    GH_TOKEN="$t"
    export GH_TOKEN
  fi
fi
exec /usr/bin/gh "$@"
