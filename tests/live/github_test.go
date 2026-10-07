//go:build live

package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/github"
)

// TestLiveGitHub uses a real repository (GITHUB_TEST_REPO=owner/name) with a
// fine-grained personal access token (GITHUB_TOKEN, contents and pull
// requests read/write on that repository only). It checks the github
// connector and the credential flow sandbox delivery uses: the delivered
// credential lets git push a branch, the platform-delivered
// pull_request.create opens a pull request that is found again by its
// marker, and gh (when installed) sees it. The branch and pull request are
// closed and deleted afterwards.
func TestLiveGitHub(t *testing.T) {
	tok, repo := os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_TEST_REPO")
	if tok == "" || repo == "" {
		t.Skip("needs GITHUB_TOKEN and GITHUB_TEST_REPO (see tests/live/README.md)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tr, err := github.New(connectors.Config{Key: "github", Adapter: "github", Secret: map[string]string{"token": tok}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Verify(ctx); err != nil {
		t.Fatalf("verify: %v", err)
	}
	info, err := tr.Invoke(ctx, "repo.read", json.RawMessage(fmt.Sprintf(`{"repo":%q}`, repo)), "live-read")
	if err != nil {
		t.Fatalf("repo.read: %v", err)
	}
	var ri struct {
		DefaultBranch string `json:"default_branch"`
	}
	_ = json.Unmarshal(info.Data, &ri)
	cred, err := tr.(connectors.CredentialIssuer).IssueCredential(ctx, connectors.CredentialScope{Repos: []string{repo}})
	if err != nil {
		t.Fatal(err)
	}

	// git with a credential helper that answers like steadmesh-tools does.
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper.sh")
	_ = os.WriteFile(helper, []byte("#!/bin/sh\n[ \"$1\" = get ] || exit 0\necho username="+cred.Username+"\necho password=\"$LIVE_TOKEN\"\n"), 0o700)
	branch := fmt.Sprintf("steadmesh-live-%d", time.Now().Unix())
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "LIVE_TOKEN="+cred.Token, "GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=4", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
			"GIT_CONFIG_KEY_1=credential.helper", "GIT_CONFIG_VALUE_1="+helper,
			"GIT_CONFIG_KEY_2=user.name", "GIT_CONFIG_VALUE_2=Steadmesh live test",
			"GIT_CONFIG_KEY_3=user.email", "GIT_CONFIG_VALUE_3=live@seats.steadmesh.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, strings.ReplaceAll(string(out), cred.Token, "***"))
		}
		return string(out)
	}
	git("clone", "--depth", "1", "https://github.com/"+repo+".git", "repo")
	dir = filepath.Join(dir, "repo")
	git("checkout", "-b", branch)
	_ = os.WriteFile(filepath.Join(dir, branch+".md"), []byte("Written by the Steadmesh live test.\n"), 0o644)
	git("add", ".")
	git("commit", "-m", "Steadmesh live test")
	git("push", "origin", branch)
	t.Cleanup(func() { deleteBranch(t, tok, repo, branch) })

	params, _ := json.Marshal(map[string]string{"repo": repo, "title": "Steadmesh live test " + branch, "head": branch, "base": ri.DefaultBranch, "body": "Opened by tests/live; closed by it too."})
	opID := "live-" + branch
	res, err := tr.Invoke(ctx, "pull_request.create", params, opID)
	if err != nil {
		t.Fatalf("pull_request.create: %v", err)
	}
	t.Cleanup(func() { closePull(t, tok, repo, res.Receipt) })
	found, err := tr.FindByOperation(ctx, "pull_request.create", params, opID)
	if err != nil || found == nil || found.Receipt != res.Receipt {
		t.Fatalf("read-back by marker: %+v %v", found, err)
	}
	if _, err := exec.LookPath("gh"); err == nil {
		cmd := exec.CommandContext(ctx, "gh", "pr", "view", res.Receipt, "--repo", repo, "--json", "headRefName")
		cmd.Env = append(os.Environ(), "GH_TOKEN="+cred.Token)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), branch) {
			t.Fatalf("gh pr view: %v %s", err, out)
		}
	}
	t.Logf("opened and read back pull request #%s on %s from %s", res.Receipt, repo, branch)
}

func githubAPI(t *testing.T, tok, method, path string, body any) {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	req, _ := http.NewRequest(method, "https://api.github.com"+path, &b)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("cleanup %s %s: %v", method, path, err)
		return
	}
	resp.Body.Close()
}

func closePull(t *testing.T, tok, repo, number string) {
	githubAPI(t, tok, http.MethodPatch, "/repos/"+repo+"/pulls/"+number, map[string]string{"state": "closed"})
}

func deleteBranch(t *testing.T, tok, repo, branch string) {
	githubAPI(t, tok, http.MethodDelete, "/repos/"+repo+"/git/refs/heads/"+branch, nil)
}
