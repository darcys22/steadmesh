#!/usr/bin/env bash
# Fetch the pinned harness CLIs for this machine into bin/harnesses, for local
# conformance runs (make conformance). Packages are fetched with `npm pack`
# (no install scripts run) and checked against the digests recorded in
# harnesses/*/CONTRACT.md before anything is extracted.
#
#   bin/harnesses/claude      Claude Code native binary
#   bin/harnesses/codex       Codex native binary
#   bin/harnesses/pi          wrapper running Pi's self-contained bundle with node
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/bin/harnesses"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in x86_64) arch=x64 ;; aarch64) arch=arm64 ;; esac
plat="$os-$arch"

CLAUDE_VERSION=2.1.289
CODEX_VERSION=0.160.1
PI_VERSION=1.0.4

# Tarball sha256 digests per platform (harnesses/*/contract/*.sha256).
claude_sum() {
  case "$1" in
    darwin-arm64) echo 0aa86cf337ede6041513a72e814e703a5d94c1e75e983f0e5b0e3f2ed1e0b700 ;;
    linux-x64) echo 50a6eb433422febcfa7b7ac1e78ba795cdf79f1fe58f017fb1d9aeb9491b337d ;;
  esac
}
codex_sum() {
  case "$1" in
    darwin-arm64) echo 93121cae6c2fe77cdb20f88f611783dbec77a3e241948ad018da120480a6f296 ;;
  esac
}
PI_SUM=04910bdae661a6529e9d6869b04006f6aad01186398b04a16c6d9793667b96c5

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# fetch <package@version> <sha256> <dir>: npm pack, verify, extract into dir.
fetch() {
  local spec="$1" want="$2" dir="$3" tmp tgz got
  [[ -n "$want" ]] || { echo "no pinned digest for $spec on $plat; add one from the contract" >&2; return 1; }
  tmp="$(mktemp -d)"
  (cd "$tmp" && npm pack --silent --ignore-scripts "$spec" >/dev/null)
  tgz="$(ls "$tmp"/*.tgz)"
  got="$(sha256 "$tgz")"
  if [[ "$got" != "$want" ]]; then
    echo "$spec: sha256 $got, want $want" >&2
    rm -rf "$tmp"
    return 1
  fi
  rm -rf "$dir" && mkdir -p "$dir"
  tar -xzf "$tgz" -C "$dir"
  rm -rf "$tmp"
}

mkdir -p "$out"
want="${*:-claude codex pi}"

if [[ " $want " == *" claude "* ]]; then
  fetch "@anthropic-ai/claude-code-$plat@$CLAUDE_VERSION" "$(claude_sum "$plat")" "$out/.claude-pkg"
  ln -sf ".claude-pkg/package/claude" "$out/claude"
  echo "claude: $("$out/claude" --version)"
fi

if [[ " $want " == *" codex "* ]]; then
  fetch "@openai/codex@$CODEX_VERSION-$plat" "$(codex_sum "$plat")" "$out/.codex-pkg"
  bin="$(find "$out/.codex-pkg/package/vendor" -path '*/bin/codex' -type f | head -1)"
  ln -sf "${bin#"$out/"}" "$out/codex"
  echo "codex: $("$out/codex" --version)"
fi

if [[ " $want " == *" pi "* ]]; then
  fetch "@earendil-works/pi-coding-agent@$PI_VERSION" "$PI_SUM" "$out/.pi-pkg"
  cat >"$out/pi" <<EOF
#!/bin/sh
exec node "$out/.pi-pkg/package/dist/bundle/cli.js" "\$@"
EOF
  chmod +x "$out/pi"
  echo "pi: $("$out/pi" --version)"
fi
