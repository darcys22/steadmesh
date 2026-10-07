//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

const vaultRootToken = "steadmesh-dev-root" // examples/foundation vault_dev_root_token default

// vault runs a Vault CLI command in the dev-mode server pod.
func vault(t *testing.T, script string) string {
	t.Helper()
	return mustRun(t, nil, "kubectl", "--context", kctx, "-n", systemNS, "exec", "vault-0", "--",
		"sh", "-c", "export VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN="+vaultRootToken+"; "+script)
}

// configureVault prepares the dev-mode Vault the foundation installed: the
// platform logs in with Kubernetes auth and reads steadmesh/* from KV v2.
// The Linear key is served from Vault for the rest of the run.
func configureVault(t *testing.T) {
	step(t, "configure dev Vault", func(t *testing.T) {
		mustRun(t, nil, "kubectl", "--context", kctx, "-n", systemNS, "wait", "--for=condition=Ready", "pod/vault-0", "--timeout=180s")
		vault(t, `vault auth enable kubernetes >/dev/null 2>&1 || true
vault write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc:443
printf 'path "secret/data/steadmesh/*" { capabilities = ["read"] }\n' | vault policy write steadmesh-platform -
vault write auth/kubernetes/role/steadmesh-platform bound_service_account_names=steadmesh-platform \
  bound_service_account_namespaces=`+systemNS+` token_policies=steadmesh-platform token_ttl=1h
vault kv put secret/steadmesh/linear api_key=lin_api_fake`)
	})
}

// credentialRotation rotates the Slack tokens in their Kubernetes Secret and
// the Linear key in Vault, revoking the old values upstream, with no
// Terraform run and no platform restart. Work that follows must use the new
// credentials.
func credentialRotation(t *testing.T, f *fakes) {
	platformPod := strings.TrimSpace(mustRun(t, nil, "kubectl", "--context", kctx, "-n", systemNS, "get", "pod",
		"-l", "app.kubernetes.io/name=steadmesh-platform", "-o", "jsonpath={.items[0].metadata.name}"))

	step(t, "rotate Slack tokens in a Kubernetes Secret", func(t *testing.T) {
		f.post(t, f.slack+"/_test/tokens", map[string]any{
			"bot": []string{"xoxb-fake-bot-token", "xoxb-rotated"}, "app": []string{"xapp-fake-app-token", "xapp-rotated"}})
		mustRun(t, nil, "kubectl", "--context", kctx, "-n", systemNS, "patch", "secret", "slack-credentials",
			"-p", `{"stringData":{"bot_token":"xoxb-rotated","app_token":"xapp-rotated"}}`)
		// Revoke the old tokens at once: anything still using them fails.
		f.post(t, f.slack+"/_test/tokens", map[string]any{"bot": []string{"xoxb-rotated"}, "app": []string{"xapp-rotated"}})
		f.dm(t, userSean, "after slack rotation", "")
		f.waitPosted(t, "D0SEAN", "ack: after slack rotation", 3*time.Minute)
		waitFor(t, "the platform to activate the rotated Slack credential", time.Minute, func() bool {
			logs, _ := run(nil, "kubectl", "--context", kctx, "-n", systemNS, "logs", platformPod)
			return strings.Contains(logs, `"msg":"connection credential activated"`) && strings.Contains(logs, `"connection":"slack"`)
		})
	})

	step(t, "rotate the Linear key in Vault", func(t *testing.T) {
		f.post(t, f.linear+"/_test/keys", map[string]any{"valid": []string{"lin_api_fake", "lin_api_rotated"}})
		vault(t, "vault kv put secret/steadmesh/linear api_key=lin_api_rotated")
		f.post(t, f.linear+"/_test/keys", map[string]any{"valid": []string{"lin_api_rotated"}})
		// Vault is polled, but the tracker's rejection of the old key makes
		// the gateway reload the secret and retry the rejected call at once.
		task := `/delegate eng_lead /task
/tool connections.invoke {"connection":"linear","operation":"project.create","params":{"name":"e2e vault rotated"}}`
		f.dm(t, userSean, task, "")
		msg := f.waitPosted(t, "D0SEAN", "e2e vault rotated", 4*time.Minute)
		if !strings.Contains(msg, "succeeded") {
			t.Fatalf("tracker call after Vault rotation did not succeed:\n%s", msg)
		}
		if n := f.projectsNamed(t, "e2e vault rotated"); n != 1 {
			t.Fatalf("tracker has %d rotated projects, want 1", n)
		}
	})

	step(t, "rotated credentials stay out of logs and runtime config", func(t *testing.T) {
		var corpus strings.Builder
		logs, _ := run(nil, "kubectl", "--context", kctx, "-n", systemNS, "logs", platformPod)
		corpus.WriteString(logs)
		corpus.WriteString(mustRun(t, nil, "kubectl", "--context", kctx, "-n", orgNS, "get",
			"agentorganizations,agentseats,configmaps,statefulsets", "-o", "yaml"))
		for _, s := range []string{"xoxb-rotated", "xapp-rotated", "lin_api_rotated", "xoxb-fake-bot-token", "lin_api_fake"} {
			if strings.Contains(corpus.String(), s) {
				t.Fatalf("credential %q found in platform logs or organisation runtime configuration", s)
			}
		}
	})
}
