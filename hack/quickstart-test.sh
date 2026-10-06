#!/usr/bin/env bash
# Tests quickstart/ the way a user runs it, on the kind cluster, with local
# builds standing in for the published release: the GitHub module sources
# point at ./modules, the chart and images are the local ones, and the
# provider is installed from a filesystem mirror instead of the registry.
# Slack and Linear are the in-repo fakes and seats run the fake harness.
# Run through `make quickstart-test`, which starts from a fresh cluster.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/out/quickstart"
kctx="kind-steadmesh"
kubectl=(kubectl --context "$kctx")
version="0.1.0"

rm -rf "$out"
mkdir -p "$out/qs"
cd "$out/qs"

echo "==> fetch the quickstart (terraform init -from-module)"
terraform init -input=false -no-color -from-module="$root/quickstart" >/dev/null
# Modules come from a git snapshot of the working tree, fetched the way
# Terraform fetches github.com sources: the whole repository is one package,
# so modules can refer to siblings (../postgres) and bundles.
src="$out/src"
mkdir -p "$src"
(cd "$root" && git ls-files -co --exclude-standard -z modules bundles | xargs -0 -I{} rsync -R {} "$src/")
git -C "$src" init -q && git -C "$src" add -A && git -C "$src" -c user.name=test -c user.email=test@example.com commit -qm snapshot
for f in $(find . -name '*.tf'); do
  perl -pi -e "s#github.com/darcys22/steadmesh//modules/([a-z-]+)\\?ref=v[0-9.]+#git::file://$src//modules/\$1#g" "$f"
done
# Local chart and images instead of the published ones.
perl -0pi -e "s#(steadmesh_version += \"[0-9.]+\")#\$1\n  chart_path = \"$root/charts/platform\"\n  image_registry = \"steadmesh\"\n  image_tag = \"dev\"#" platform/main.tf
if grep -rq 'github.com/darcys22' --include='*.tf' .; then
  echo "unrewritten module source" >&2; exit 1
fi

echo "==> provider mirror"
plat="$(go env GOOS)_$(go env GOARCH)"
pkg="$out/mirror/registry.terraform.io/darcys22/steadmesh/$version/$plat"
mkdir -p "$pkg"
cp "$root/bin/terraform-provider-steadmesh" "$pkg/terraform-provider-steadmesh_v$version"
cat > "$out/terraformrc" <<EOF
provider_installation {
  filesystem_mirror {
    path    = "$out/mirror"
    include = ["darcys22/steadmesh"]
  }
  direct {
    exclude = ["darcys22/steadmesh"]
  }
}
EOF
export TF_CLI_CONFIG_FILE="$out/terraformrc" TF_IN_AUTOMATION=1 TF_INPUT=0

echo "==> platform stage"
cat > platform/terraform.tfvars <<EOF
kube_context      = "$kctx"
slack_bot_token   = "xoxb-fake-bot-token"
slack_app_token   = "xapp-fake-app-token"
anthropic_api_key = "sk-ant-fake"
linear_api_key    = "lin_api_fake"
enable_console    = true
EOF
terraform -chdir=platform init -no-color >"$out/platform.init.log"
terraform -chdir=platform apply -auto-approve -no-color >"$out/platform.apply.log" || { tail -30 "$out/platform.apply.log"; exit 1; }

echo "==> fake Slack and Linear"
"${kubectl[@]}" apply -f "$root/tests/e2e/manifests/fakes.yaml" >/dev/null
"${kubectl[@]}" -n steadmesh-system rollout status deploy/steadmesh-fakes --timeout=180s >/dev/null

echo "==> organisation stage"
fakes="http://steadmesh-fakes.steadmesh-system.svc"
cat > organisation/terraform.tfvars <<EOF
slack_workspace_id  = "T0FAKE"
linear_team_id      = "TEAM-FAKE"
humans              = { sean = { slack_user_id = "U0SEAN", display_name = "Sean's representative" } }
harness             = "fake"
slack_endpoint_ref  = "$fakes:8090"
linear_endpoint_ref = "$fakes:8091"
EOF
terraform -chdir=organisation init -no-color >"$out/organisation.init.log"
terraform -chdir=organisation apply -auto-approve -no-color >"$out/organisation.apply.log" || { tail -30 "$out/organisation.apply.log"; exit 1; }

echo "==> message the representative"
"${kubectl[@]}" -n steadmesh-system port-forward svc/steadmesh-fakes 18090:8090 18091:8091 >"$out/port-forward.log" 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
for _ in $(seq 60); do curl -sf localhost:18090/_test/posted >/dev/null && break; sleep 1; done

wait_posted() { # substring, seconds
  local deadline=$((SECONDS + $2))
  while ((SECONDS < deadline)); do
    if curl -sf localhost:18090/_test/posted | jq -e --arg s "$1" 'any(.[]; .text | contains($s))' >/dev/null; then
      return 0
    fi
    sleep 3
  done
  echo "timed out waiting for a post containing: $1" >&2
  curl -s localhost:18090/_test/posted | jq -r '.[] | "\(.channel): \(.text)"' >&2
  return 1
}

curl -sf localhost:18090/_test/dm -d '{"user":"U0SEAN","text":"hello from the quickstart"}' >/dev/null
wait_posted "ack: hello from the quickstart" 240

curl -sf localhost:18090/_test/dm -d @- >/dev/null <<'JSON'
{"user":"U0SEAN","text":"/delegate eng_lead /task\n/tool connections.invoke {\"connection\":\"linear\",\"operation\":\"project.create\",\"params\":{\"name\":\"quickstart project\"}}"}
JSON
wait_posted "Update from eng_lead" 300
curl -sf localhost:18091/_test/state | jq -e 'any(.projects[]; .name == "quickstart project")' >/dev/null || {
  echo "the delegated tracker project was not created" >&2; exit 1; }

echo "PASS: quickstart installed the platform, started the agents and completed a delegated task"
