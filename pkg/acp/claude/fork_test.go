package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestForkSessionPreservesRequestedConfiguration(t *testing.T) {
	a := newContractAgent(t).(*Agent)
	t.Cleanup(func() { _ = a.Close() })
	cwd := t.TempDir()
	additional := t.TempDir()
	server := acp.McpServer{Http: &acp.McpServerHttpInline{
		Name: "remote", Type: "http", Url: "https://example.test/mcp",
		Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer " + strings.Repeat("test-token", 20000)}},
	}}

	resp, err := a.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:             "source-session",
		Cwd:                   cwd,
		AdditionalDirectories: []string{additional},
		McpServers:            []acp.McpServer{server},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := a.lookup(resp.SessionId)
	if s == nil {
		t.Fatal("forked session was not registered")
	}
	s.mu.Lock()
	mcpCount := len(s.mcpServers)
	resumeFrom, forkOnResume := s.resumeFrom, s.forkOnResume
	s.mu.Unlock()
	if resumeFrom != "source-session" || !forkOnResume {
		t.Fatalf("fork state = resume %q, fork %v", resumeFrom, forkOnResume)
	}
	proc, err := s.ensureProc()
	if err != nil {
		t.Fatal(err)
	}
	args := proc.cmd.Args
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "test-token") || strings.Contains(joined, server.Http.Url) {
		t.Fatal("MCP credentials or server URL appeared in argv")
	}
	i := slices.Index(args, "--mcp-config")
	if mcpCount != 1 || i < 0 || i+1 >= len(args) {
		t.Fatal("fork lost MCP configuration")
	}
	path := args[i+1]
	config, err := os.ReadFile(path)
	if err != nil || string(config) != mcpConfigJSON([]acp.McpServer{server}) {
		t.Fatalf("forked process did not receive the exact MCP configuration: %v", err)
	}
	if st, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
		t.Fatal("MCP config must be private")
	}
	if !strings.Contains(joined, additional) {
		t.Fatalf("fork lost additional directory: %s", joined)
	}
	s.close()
	<-proc.dead
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("MCP config survived process exit: %v", err)
	}
}

func TestFailedLaunchRemovesMCPConfig(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	a := New(Options{Path: filepath.Join(dir, "missing")})
	s := a.newSession("s", dir, "", "", nil)
	s.mcpServers = []acp.McpServer{{Http: &acp.McpServerHttpInline{Name: "remote", Url: "https://example.test/mcp"}}}
	if _, err := s.ensureProc(); err == nil {
		t.Fatal("missing executable started")
	}
	if files, err := os.ReadDir(dir); err != nil || len(files) != 0 {
		t.Fatalf("failed launch left files %v, err %v", files, err)
	}
}
