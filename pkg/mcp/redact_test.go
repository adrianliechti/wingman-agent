package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestRedactRemovesURLCredentialsAndKeepsTheCause(t *testing.T) {
	cause := &url.Error{Op: "Post", URL: "https://user:pass@mcp.example.com/mcp?apiKey=SECRET&x=1#frag", Err: context.DeadlineExceeded}
	err := Redact(fmt.Errorf("remote: %w", cause))
	if got, want := err.Error(), `remote: Post "https://mcp.example.com/mcp?[redacted]": context deadline exceeded`; got != want {
		t.Fatalf("message = %s", got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cause was lost")
	}
	plain := errors.New("fork/exec /nonexistent/mcp-server: no such file or directory")
	if Redact(plain) != plain || Redact(nil) != nil {
		t.Fatal("an error without a URL changed")
	}
	if got := Redact(errors.New("GET http://localhost:8080/mcp failed")).Error(); got != "GET http://localhost:8080/mcp failed" {
		t.Fatalf("a URL without credentials changed: %s", got)
	}
}
