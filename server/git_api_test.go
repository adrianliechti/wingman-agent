package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	types "github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func TestGitAPIStageCommitPushAndPull(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.User.Name = "Wingman Test"
	cfg.User.Email = "wingman@test.local"
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := New(context.Background(), repoDir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	status := getGitStatus(t, web.URL)
	if status.Head != "" {
		t.Fatalf("unborn HEAD = %q, want empty", status.Head)
	}
	if len(status.Files) != 1 || status.Files[0].Path != "hello.txt" || !status.Files[0].Changed {
		t.Fatalf("initial status = %+v", status)
	}
	postGit(t, web.URL, "stage", `{"paths":["hello.txt"]}`)
	status = getGitStatus(t, web.URL)
	if len(status.Files) != 1 || !status.Files[0].Staged {
		t.Fatalf("staged status = %+v", status)
	}
	postGit(t, web.URL, "unstage", `{"paths":["hello.txt"]}`)
	status = getGitStatus(t, web.URL)
	if len(status.Files) != 1 || status.Files[0].Staged || !status.Files[0].Changed {
		t.Fatalf("unstaged status = %+v", status)
	}
	postGit(t, web.URL, "stage", `{"paths":["hello.txt"]}`)
	postGit(t, web.URL, "commit", `{"message":"initial commit"}`)
	if status = getGitStatus(t, web.URL); len(status.Files) != 0 {
		t.Fatalf("status after commit = %+v", status)
	}
	head, err := repo.Head()
	if err != nil || status.Head != head.Hash().String() {
		t.Fatalf("status HEAD = %q, repository HEAD = %v, err = %v", status.Head, head, err)
	}
	mainBranch := status.Branch
	postGit(t, web.URL, "branches", `{"name":"feature/web-picker"}`)
	if status = getGitStatus(t, web.URL); status.Branch != "feature/web-picker" {
		t.Fatalf("status after branch creation = %+v", status)
	}
	branches := getGitBranches(t, web.URL)
	if len(branches.Branches) != 2 || !branches.Branches[0].Current && !branches.Branches[1].Current {
		t.Fatalf("branches = %+v", branches)
	}
	postGit(t, web.URL, "checkout", `{"name":"`+mainBranch+`"}`)
	if status = getGitStatus(t, web.URL); status.Branch != mainBranch {
		t.Fatalf("status after checkout = %+v", status)
	}

	remoteDir := t.TempDir()
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remoteDir}}); err != nil {
		t.Fatal(err)
	}
	postGit(t, web.URL, "push", "")
	status = getGitStatus(t, web.URL)
	if status.Upstream == "" {
		t.Fatalf("push did not establish upstream: %+v", status)
	}
	postGit(t, web.URL, "pull", "")
}

func getGitBranches(t *testing.T, baseURL string) GitBranches {
	t.Helper()
	var branches GitBranches
	gitCall(t, baseURL, "git/branches", map[string]any{}, &branches)
	return branches
}

func TestGitAPIBranchFetchIsExplicit(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.User.Name = "Wingman Test"
	cfg.User.Email = "wingman@test.local"
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	missingRemote := filepath.Join(t.TempDir(), "missing.git")
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{missingRemote}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := New(context.Background(), repoDir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	postGit(t, web.URL, "stage", "")
	postGit(t, web.URL, "commit", `{"message":"initial commit"}`)
	if branches := getGitBranches(t, web.URL); branches.Warning != "" {
		t.Fatalf("read-only branch listing attempted a fetch: %q", branches.Warning)
	}

	var refreshed GitBranches
	gitCall(t, web.URL, "git/branches", map[string]any{"refresh": true}, &refreshed)
	if refreshed.Warning == "" {
		t.Fatal("fetch against a missing remote returned no warning")
	}
}

func TestGitAPIInitCreatesRepository(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := New(context.Background(), workDir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	errRPC := gitCallError(t, web.URL, "git/status", map[string]any{}, nil)
	if errRPC == nil || errRPC.Status != 404 {
		t.Fatalf("status before init: %v", errRPC)
	}
	caps := getWorkspaceInfo(t, web.URL)
	assertWorkspaceInfo(t, caps)
	if getGitStatusNullable(t, web.URL) != nil {
		t.Fatalf("workspace before init = %v", caps)
	}

	postGit(t, web.URL, "init", "")
	caps = getWorkspaceInfo(t, web.URL)
	if getGitStatusNullable(t, web.URL) == nil {
		t.Fatalf("workspace after init = %v", caps)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err != nil {
		t.Fatalf("missing .git directory: %v", err)
	}
	status := getGitStatus(t, web.URL)
	if status.Branch != "main" {
		t.Fatalf("branch after init = %q, want main", status.Branch)
	}
	if len(status.Files) != 1 || status.Files[0].Path != "hello.txt" || !status.Files[0].Changed {
		t.Fatalf("status after init = %+v", status)
	}

	errRPC = gitCallError(t, web.URL, "git/run", map[string]any{"action": "init"}, nil)
	if errRPC == nil || errRPC.Status != 409 {
		t.Fatalf("second init: %v", errRPC)
	}
}

func TestGitAPIInitRemovesDanglingShadowPointer(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	workDir := t.TempDir()
	stale := "gitdir: " + filepath.Join(t.TempDir(), "gone", "changes-old.git") + "\n"
	if err := os.WriteFile(filepath.Join(workDir, ".git"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := New(context.Background(), workDir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	caps := getWorkspaceInfo(t, web.URL)
	if getGitStatusNullable(t, web.URL) != nil {
		t.Fatalf("capabilities = %v", caps)
	}
	postGit(t, web.URL, "init", "")
	info, err := os.Stat(filepath.Join(workDir, ".git"))
	if err != nil || !info.IsDir() {
		t.Fatalf(".git after init: info=%v err=%v", info, err)
	}
	if status := getGitStatus(t, web.URL); status.Branch != "main" {
		t.Fatalf("status after init = %+v", status)
	}
}

func TestGitAPIRejectsInvalidPath(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	repoDir := t.TempDir()
	if _, err := git.PlainInit(repoDir, false); err != nil {
		t.Fatal(err)
	}
	app, err := New(context.Background(), repoDir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	client := ahpTestClient(t, "ws"+strings.TrimPrefix(web.URL, "http")+"/ahp", "path-test")
	var result any
	err = client.Request(t.Context(), "invokeChangesetOperation", map[string]any{"channel": ahpUnstagedChanges, "operationId": "stage", "target": map[string]any{"kind": "resource", "resource": "file:///outside"}}, &result)
	if err == nil {
		t.Fatal("outside path accepted")
	}

}

func TestGitAPIStageAll(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	repoDir := t.TempDir()
	if _, err := git.PlainInit(repoDir, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(repoDir, path), []byte(path+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app, err := New(context.Background(), repoDir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	postGit(t, web.URL, "stage", "")
	status := getGitStatus(t, web.URL)
	if len(status.Files) != 2 || !status.Files[0].Staged || !status.Files[1].Staged {
		t.Fatalf("stage-all status = %+v", status)
	}
}

func TestGitAPIRejectsLooseJSON(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	repoDir := t.TempDir()
	if _, err := git.PlainInit(repoDir, false); err != nil {
		t.Fatal(err)
	}
	app, err := New(context.Background(), repoDir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	for name, request := range map[string]struct{ path, body string }{
		"unknown field":      {"commits", `{"message":"test","recursive":true}`},
		"trailing value":     {"commits", `{"message":"test"} {}`},
		"wrong action field": {"pull", `{"message":"test"}`},
		"empty commit":       {"commits", `{"message":""}`},
		"action override":    {"commits", `{"action":"pull","message":"test"}`},
	} {
		t.Run(name, func(t *testing.T) {
			response, err := http.Post(web.URL+"/api/workspace/git/"+request.path, "application/json", strings.NewReader(request.body))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid request returned %d", response.StatusCode)
			}
		})
	}
}

func TestGitAPIHistoryAndCompare(t *testing.T) {
	t.Setenv("WINGMAN_URL", "http://localhost:1")
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.User.Name = "Wingman Test"
	cfg.User.Email = "wingman@test.local"
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := New(context.Background(), repoDir, &ServerOptions{NoBrowser: true, disableManagedTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	web := httptest.NewServer(scopedTestHandler(app))
	defer web.Close()

	postGit(t, web.URL, "stage", `{"paths":["base.txt"]}`)
	postGit(t, web.URL, "commit", `{"message":"initial"}`)
	mainBranch := getGitStatus(t, web.URL).Branch
	postGit(t, web.URL, "branches", `{"name":"feature/compare"}`)
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	postGit(t, web.URL, "stage", `{"paths":["feature.txt"]}`)
	postGit(t, web.URL, "commit", `{"message":"draft feature"}`)
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	postGit(t, web.URL, "stage", `{"paths":["feature.txt"]}`)
	postGit(t, web.URL, "commit", `{"message":"feature commit"}`)

	var history []GitCommit
	gitCall(t, web.URL, "git/history", map[string]any{}, &history)
	foundFeature := false
	foundUnreferenced := false
	initialHash := ""
	for _, commit := range history {
		foundFeature = foundFeature || commit.Summary == "feature commit"
		foundUnreferenced = foundUnreferenced || commit.Refs != nil && len(commit.Refs) == 0
		if commit.Summary == "initial" {
			initialHash = commit.Hash
		}
	}
	if len(history) != 3 || !foundFeature || !foundUnreferenced || initialHash == "" {
		t.Fatalf("history = %+v", history)
	}
	insightsResponse, err := http.Get(web.URL + "/api/workspace/insights")
	if err != nil {
		t.Fatal(err)
	}
	defer insightsResponse.Body.Close()
	var insights struct {
		Commits int `json:"commits"`
	}
	if err := json.NewDecoder(insightsResponse.Body).Decode(&insights); err != nil {
		t.Fatal(err)
	}
	if insightsResponse.StatusCode != http.StatusOK || insights.Commits != len(history) {
		t.Fatalf("workspace insights = %+v, status %d", insights, insightsResponse.StatusCode)
	}

	rootComparison := getGitComparison(t, web.URL, ":empty", initialHash, "direct")
	if len(rootComparison.Files) != 1 || rootComparison.Files[0].Path != "base.txt" || rootComparison.Files[0].Status != "added" {
		t.Fatalf("root comparison = %+v", rootComparison)
	}

	comparison := getGitComparison(t, web.URL, mainBranch, "feature/compare", "merge-base")
	if comparison.MergeBaseHash == "" || len(comparison.Files) != 1 || comparison.Files[0].Path != "feature.txt" || !strings.Contains(comparison.Files[0].Modified, "feature") {
		t.Fatalf("comparison = %+v", comparison)
	}

	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("working feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "local.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	comparison = getGitComparison(t, web.URL, mainBranch, ":worktree", "merge-base")
	if comparison.Head != ":worktree" || comparison.MergeBaseHash == "" || len(comparison.Files) != 2 {
		t.Fatalf("worktree comparison = %+v", comparison)
	}
	files := map[string]DiffEntry{}
	for _, file := range comparison.Files {
		files[file.Path] = file
	}
	if files["feature.txt"].Modified != "working feature\n" || files["local.txt"].Status != "added" {
		t.Fatalf("worktree files = %+v", files)
	}
}

func getWorkspaceInfo(t *testing.T, baseURL string) map[string]any {
	t.Helper()
	res, err := http.Get(baseURL + "/api/workspace")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("workspace endpoint = %d", res.StatusCode)
	}
	var caps map[string]any
	if err := json.NewDecoder(res.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	return caps
}

func assertWorkspaceInfo(t *testing.T, caps map[string]any) {
	t.Helper()
	for _, name := range []string{"lsp", "debug"} {
		if _, ok := caps[name].(bool); !ok {
			t.Errorf("capability %q = %#v, want non-null boolean", name, caps[name])
		}
	}
	for _, name := range []string{"diffs", "git_init", "tasks", "terminal", "workspace_name", "git", "backends"} {
		if _, exists := caps[name]; exists {
			t.Errorf("redundant field %q remains", name)
		}
	}
	if _, ok := caps["shells"].([]any); !ok {
		t.Fatalf("shells must be a non-null array: %v", caps["shells"])
	}

	for _, name := range []string{"platform", "name", "path", "workspaceId", "instanceId"} {
		if _, ok := caps[name].(string); !ok {
			t.Errorf("capability %q = %#v, want non-null string", name, caps[name])
		}
	}
}

func getGitStatusNullable(t *testing.T, baseURL string) *GitStatus {
	t.Helper()
	var status *GitStatus
	response, err := http.Get(baseURL + "/api/workspace/git")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func getGitStatus(t *testing.T, baseURL string) GitStatus {
	t.Helper()
	var status GitStatus
	gitCall(t, baseURL, "git/status", map[string]any{}, &status)
	return status
}

func postGit(t *testing.T, baseURL, action, body string) {
	t.Helper()
	params := map[string]any{}
	if body != "" {
		if err := json.Unmarshal([]byte(body), &params); err != nil {
			t.Fatal(err)
		}
	}
	if action == "stage" || action == "unstage" {
		channel := ahpUnstagedChanges
		if action == "unstage" {
			channel = ahpStagedChanges
		}
		client := ahpTestClient(t, "ws"+strings.TrimPrefix(baseURL, "http")+"/ahp", "git-"+action+body)
		result, sub, err := client.Subscribe(t.Context(), channel)
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		invoke := func(target any) {
			request := map[string]any{"channel": channel, "operationId": action}
			if target != nil {
				request["target"] = target
			}
			if err := client.Request(t.Context(), "invokeChangesetOperation", request, nil); err != nil {
				t.Fatal(err)
			}
		}
		if paths, ok := params["paths"].([]any); ok {
			for _, path := range paths {
				var target *types.ChangesetOperationResourceTarget
				for _, file := range result.Snapshot.State.Changeset.Files {
					if strings.HasSuffix(file.Id, "/"+path.(string)) {
						target = &types.ChangesetOperationResourceTarget{Kind: "resource", Resource: file.Id, Nonce: file.Nonce}
						break
					}
				}
				if target == nil {
					t.Fatalf("change not found: %v", path)
				}
				invoke(target)
			}
		} else {
			invoke(nil)
		}
		return
	}
	if action == "branches" {
		action = "createBranch"
	}
	params["action"] = action
	gitCall(t, baseURL, "git/run", params, nil)
}

func gitCall(t *testing.T, baseURL, method string, params any, target any) {
	t.Helper()
	if err := gitCallError(t, baseURL, method, params, target); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}
func gitCallError(t *testing.T, baseURL, method string, params any, target any) *gitHTTPError {
	t.Helper()
	p, _ := params.(map[string]any)
	verb, path := http.MethodGet, ""
	body := map[string]any{}
	switch method {
	case "git/status":
		path = "/api/workspace/git"
	case "git/history":
		path = "/api/workspace/git/commits"
	case "git/branches":
		path = "/api/workspace/git/branches"
		if p["refresh"] == true {
			verb, path = http.MethodPost, "/api/workspace/git/fetch"
		}
	case "git/diff":
		query := url.Values{}
		for k, v := range p {
			query.Set(k, v.(string))
		}
		path = "/api/workspace/git/diff?" + query.Encode()
	case "git/run":
		verb = http.MethodPost
		action := p["action"].(string)
		endpoint := map[string]string{"createBranch": "branches", "commit": "commits"}[action]
		if endpoint == "" {
			endpoint = action
		}
		path = "/api/workspace/git/" + endpoint
		for k, v := range p {
			if k != "action" {
				body[k] = v
			}
		}
	default:
		t.Fatalf("unknown test operation: %s", method)
	}
	data, _ := json.Marshal(body)
	request, err := http.NewRequestWithContext(t.Context(), verb, baseURL+path, strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		return &gitHTTPError{Status: response.StatusCode, Message: string(data)}
	}
	if method == "git/status" && string(bytes.TrimSpace(data)) == "null" {
		return &gitHTTPError{Status: 404, Message: "not a Git repository"}
	}
	if target != nil {
		if err := json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	return nil
}

type gitHTTPError struct {
	Status  int
	Message string
}

func (e *gitHTTPError) Error() string { return e.Message }

type GitCompare struct {
	Base          string      `json:"base"`
	Head          string      `json:"head"`
	BaseHash      string      `json:"base_hash"`
	HeadHash      string      `json:"head_hash"`
	MergeBaseHash string      `json:"merge_base_hash,omitempty"`
	Files         []DiffEntry `json:"files"`
}

func getGitComparison(t *testing.T, baseURL, base, head, mode string) GitCompare {
	t.Helper()
	var result struct {
		GitCompare
		Channel string `json:"channel"`
	}
	gitCall(t, baseURL, "git/diff", map[string]any{"base": base, "head": head, "mode": mode}, &result)
	client := ahpTestClient(t, "ws"+strings.TrimPrefix(baseURL, "http")+"/ahp", "comparison"+result.Channel)
	snapshot, sub, err := client.Subscribe(t.Context(), result.Channel)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	read := func(side *types.FileEditSide) string {
		if side == nil {
			return ""
		}
		var content types.ResourceReadResult
		if err := client.Request(t.Context(), "resourceRead", map[string]any{"channel": "ahp-root://", "uri": side.Content.Uri}, &content); err != nil {
			t.Fatal(err)
		}
		return content.Data
	}
	for _, file := range snapshot.Snapshot.State.Changeset.Files {
		path := file.Id[strings.LastIndex(file.Id, "/")+1:]
		status := "modified"
		if file.Edit.Before == nil {
			status = "added"
		}
		if file.Edit.After == nil {
			status = "deleted"
		}
		result.Files = append(result.Files, DiffEntry{Path: path, Status: status, Original: read(file.Edit.Before), Modified: read(file.Edit.After)})
	}
	return result.GitCompare
}
