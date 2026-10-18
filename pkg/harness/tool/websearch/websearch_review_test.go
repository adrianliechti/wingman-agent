package websearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockSearch(t *testing.T, f roundTripFunc) {
	t.Helper()
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: f}
	t.Cleanup(func() { http.DefaultClient = old })
}

func searchResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func resultHTML(title, target, snippet string) string {
	return fmt.Sprintf(`<div class="result"><a class="result__a" href="%s">%s</a><a class="result__snippet">%s</a></div>`, target, title, snippet)
}

func TestSearchRejectsIncompleteOrUnexpectedResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"challenge", 200, `<form id="challenge-form"><div class="anomaly-modal">Verify you are human</div></form>`},
		{"unknown HTML", 200, `<html><body>Temporarily unavailable</body></html>`},
		{"empty", 200, ""},
		{"oversized", 200, resultHTML("Title", "https://example.com/", "snippet") + strings.Repeat(" ", maxReadBytes)},
		{"unresolved redirect", 302, ""},
		{"partial", 206, resultHTML("Title", "https://example.com/", "snippet")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockSearch(t, func(*http.Request) (*http.Response, error) { return searchResponse(tc.status, tc.body), nil })
			result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": "test"})
			if err == nil {
				t.Fatalf("response treated as successful: %.200s", result.Content)
			}
		})
	}
}

func TestSearchTrueNoResults(t *testing.T) {
	mockSearch(t, func(*http.Request) (*http.Response, error) {
		return searchResponse(200, `<div class="result result--no-result"><div class="no-results">No results found</div></div>`), nil
	})
	result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": "unlikely query"})
	if err != nil || !strings.Contains(result.Content, "No results") {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestSearchSnippetsStayWithTheirResult(t *testing.T) {
	page := resultHTML("First", "https://first.example/", "") +
		resultHTML("Ad", "https://duckduckgo.com/y.js?ad_domain=ad.example", "Wrong ad snippet") +
		resultHTML("Second", "https://second.example/", "Second snippet") +
		resultHTML("Duplicate", "https://second.example/", "Duplicate snippet")
	mockSearch(t, func(*http.Request) (*http.Response, error) { return searchResponse(200, page), nil })
	result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Content, "Wrong ad snippet") || strings.Contains(result.Content, "Duplicate") || !strings.Contains(result.Content, "Second snippet") {
		t.Fatalf("misassociated or duplicate results: %s", result.Content)
	}
}

func TestSearchURLValidation(t *testing.T) {
	for href, want := range map[string]string{
		"/l/?uddg=https%3A%2F%2Fexample.com%2Fpage":                "https://example.com/page",
		"/l?uddg=https%3A%2F%2Fexample.com%2Fpage":                 "https://example.com/page",
		"  https://example.com/page  ":                             "https://example.com/page",
		"https://notduckduckgo.com/page":                           "https://notduckduckgo.com/page",
		"https://DUCKDUCKGO.COM/l/?uddg=https%3A%2F%2Fexample.com": "https://example.com",
		"https://duckduckgo.com/about":                             "https://duckduckgo.com/about",
		"https:///missing-host":                                    "",
		"https:opaque":                                             "",
		"https://example.com:99999/":                               "",
		"https://user:password@example.com/":                       "",
		"https://example.com/\x00bad":                              "",
	} {
		if got := resolveResultURL(href); got != want {
			t.Errorf("resolveResultURL(%q) = %q, want %q", href, got, want)
		}
	}
}

func TestSearchRestrictsProviderRedirects(t *testing.T) {
	for _, target := range []string{"https://example.com/?q=test", "http://html.duckduckgo.com/html/", "https://duckduckgo.com:8443/", "https://notduckduckgo.com/"} {
		t.Run(target, func(t *testing.T) {
			requests := 0
			mockSearch(t, func(*http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					r := searchResponse(302, "")
					r.Header.Set("Location", target)
					return r, nil
				}
				return searchResponse(200, resultHTML("Title", "https://example.com", "Snippet")), nil
			})
			_, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": "test"})
			if err == nil || requests != 1 {
				t.Fatalf("followed unapproved redirect: requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestSearchCancellationBeforeApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	prompts := 0
	elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) { prompts++; return false, nil }}
	_, err := Tools(elicit)[0].Execute(ctx, map[string]any{"query": "test"})
	if !errors.Is(err, context.Canceled) || prompts != 0 {
		t.Fatalf("canceled search prompted: prompts=%d err=%v", prompts, err)
	}
}

func TestSearchConcurrentApproval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) { calls.Add(1); <-release; return true, nil }}
		approval := &searchApproval{}
		var group sync.WaitGroup
		for range 16 {
			group.Go(func() {
				if err := approveSearch(t.Context(), elicit, approval, "query"); err != nil {
					t.Error(err)
				}
			})
		}
		synctest.Wait()
		close(release)
		group.Wait()
		if calls.Load() != 1 {
			t.Fatalf("prompted %d times for concurrent searches", calls.Load())
		}
	})
}

func TestSearchCanceledApprovalNotRemembered(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) { cancel(); return true, nil }}
	approval := &searchApproval{}
	if err := approveSearch(ctx, elicit, approval, "query"); !errors.Is(err, context.Canceled) || approval.allowed {
		t.Fatalf("canceled approval remembered: allowed=%v err=%v", approval.allowed, err)
	}
}

func TestSearchOutputBudget(t *testing.T) {
	var page strings.Builder
	for i := range 10 {
		page.WriteString(resultHTML(strings.Repeat("題", 1000), fmt.Sprintf("https://example.com/%d", i), strings.Repeat("文", 10000)))
	}
	mockSearch(t, func(*http.Request) (*http.Response, error) { return searchResponse(200, page.String()), nil })
	result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": "test", "max_results": 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) > tool.MaxInlineResultBytes || !utf8.ValidString(result.Content) || !strings.Contains(result.Content, "truncated") {
		t.Fatalf("unbounded or unmarked search output: bytes=%d", len(result.Content))
	}
	for i := range 10 {
		if !strings.Contains(result.Content, fmt.Sprintf("https://example.com/%d", i)) {
			t.Errorf("lost result %d", i)
		}
	}
}

func TestSearchRequestAndLimits(t *testing.T) {
	for _, limit := range []int{0, 1, 100} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			mockSearch(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("q") != `quoted "error" & C++` || r.Header.Get("Accept") != "text/html" {
					t.Fatalf("bad request: %s", r.URL)
				}
				var b strings.Builder
				for i := range 15 {
					b.WriteString(resultHTML(fmt.Sprint(i), fmt.Sprintf("https://example.com/%d", i), "Snippet"))
				}
				return searchResponse(200, b.String()), nil
			})
			args := map[string]any{"query": ` quoted "error" & C++ `}
			want := defaultResults
			if limit != 0 {
				args["max_results"] = limit
				want = min(limit, maxResults)
			}
			result, err := Tools(nil)[0].Execute(t.Context(), args)
			if err != nil || strings.Count(result.Content, "https://example.com/") != want {
				t.Fatalf("limit=%d result=%#v err=%v", limit, result, err)
			}
		})
	}
}

func TestSearchInvalidInputBeforeApproval(t *testing.T) {
	prompts := 0
	elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) { prompts++; return false, nil }}
	for _, args := range []map[string]any{
		{"query": strings.Repeat("q", maxQueryRunes+1)},
		{"query": "bad\x00query"},
		{"query": "bad\x1b[31mquery"},
		{"query": "bad\xffquery"},
		{"query": "test", "max_results": nil},
		{"query": "test", "max_results": 1.5},
		{"query": "test", "max_results": "2"},
	} {
		if _, err := Tools(elicit)[0].Execute(t.Context(), args); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	if prompts != 0 {
		t.Fatalf("invalid input triggered %d approvals", prompts)
	}
}

func TestSearchCharsetAndContentType(t *testing.T) {
	for _, tc := range []struct {
		contentType, title, want string
		fail                     bool
	}{
		{"text/html; charset=iso-8859-1", "Caf\xe9", "Café", false},
		{"Text/HTML; charset=utf-8", "世界", "世界", false},
		{"", "世界", "世界", false},
		{"application/json", "Title", "", true},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			mockSearch(t, func(*http.Request) (*http.Response, error) {
				r := searchResponse(200, resultHTML(tc.title, "https://example.com/", ""))
				r.Header.Set("Content-Type", tc.contentType)
				return r, nil
			})
			result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": "test"})
			if tc.fail {
				if err == nil {
					t.Fatal("unexpected content type accepted")
				}
				return
			}
			if err != nil || !strings.Contains(result.Content, tc.want) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestSearchResponseAtSourceLimit(t *testing.T) {
	page := resultHTML("Title", "https://example.com/", "Snippet")
	page += strings.Repeat(" ", maxReadBytes-len(page))
	mockSearch(t, func(*http.Request) (*http.Response, error) { return searchResponse(200, page), nil })
	if _, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": strings.Repeat("界", maxQueryRunes)}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchLongURLsStayCompleteOrAreOmitted(t *testing.T) {
	tooLong := "https://example.com/" + strings.Repeat("a", tool.MaxInlineResultBytes)
	for _, another := range []bool{false, true} {
		t.Run(fmt.Sprint(another), func(t *testing.T) {
			page := resultHTML("Too long", tooLong, "")
			if another {
				page += resultHTML("Fits", "https://example.com/fits", "")
			}
			mockSearch(t, func(*http.Request) (*http.Response, error) { return searchResponse(200, page), nil })
			result, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": "test"})
			if !another {
				if err == nil {
					t.Fatal("unusable URL should return an error")
				}
				return
			}
			if err != nil || !strings.Contains(result.Content, "https://example.com/fits") || !strings.Contains(result.Content, "1 results omitted") || len(result.Content) > tool.MaxInlineResultBytes {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestSearchProviderRedirectAndHopLimit(t *testing.T) {
	for _, loop := range []bool{false, true} {
		t.Run(fmt.Sprint(loop), func(t *testing.T) {
			requests := 0
			mockSearch(t, func(*http.Request) (*http.Response, error) {
				requests++
				if loop || requests == 1 {
					r := searchResponse(302, "")
					r.Header.Set("Location", "https://duckduckgo.com/html/")
					return r, nil
				}
				return searchResponse(200, resultHTML("Title", "https://example.com/", "Snippet")), nil
			})
			_, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": "test"})
			if loop {
				if err == nil || requests != 10 {
					t.Fatalf("redirect loop: requests=%d err=%v", requests, err)
				}
			} else if err != nil || requests != 2 {
				t.Fatalf("valid redirect: requests=%d err=%v", requests, err)
			}
		})
	}
	target := "https://example.com/"
	for range 20 {
		target = "https://duckduckgo.com/l/?uddg=" + url.QueryEscape(target)
	}
	if got := resolveResultURL(target); got != "" {
		t.Fatal("excessively nested result redirect accepted")
	}
}

type cancelBody struct {
	io.Reader
	cancel context.CancelFunc
	closed bool
}

func (b *cancelBody) Read(p []byte) (int, error) { b.cancel(); return b.Reader.Read(p) }
func (b *cancelBody) Close() error               { b.closed = true; return nil }

func TestSearchBodyCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := &cancelBody{Reader: strings.NewReader(resultHTML("Title", "https://example.com/", "Snippet")), cancel: cancel}
	mockSearch(t, func(*http.Request) (*http.Response, error) {
		r := searchResponse(200, "")
		r.Body = body
		return r, nil
	})
	if _, err := Tools(nil)[0].Execute(ctx, map[string]any{"query": "test"}); !errors.Is(err, context.Canceled) || !body.closed {
		t.Fatalf("body cancellation: closed=%v err=%v", body.closed, err)
	}
}

func TestSearchTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mockSearch(t, func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
		started := time.Now()
		_, err := Tools(nil)[0].Execute(t.Context(), map[string]any{"query": "test"})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != searchTimeout {
			t.Fatalf("timeout: elapsed=%s err=%v", time.Since(started), err)
		}
	})
}

func TestSearchWaitingApprovalCancelsIndependently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		elicit := &tool.Elicitation{Confirm: func(context.Context, string) (bool, error) { <-release; return true, nil }}
		approval := &searchApproval{}
		first := make(chan error, 1)
		go func() { first <- approveSearch(t.Context(), elicit, approval, "first") }()
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := approveSearch(ctx, elicit, approval, "second"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting search: %v", err)
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := approveSearch(t.Context(), elicit, approval, "third"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSearchCanceledPromptOwnerDoesNotCancelWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		elicit := &tool.Elicitation{Confirm: func(ctx context.Context, _ string) (bool, error) {
			calls++
			if calls == 1 {
				<-ctx.Done()
				return false, ctx.Err()
			}
			return true, nil
		}}
		approval := &searchApproval{}
		first, second := make(chan error, 1), make(chan error, 1)
		go func() { first <- approveSearch(ctx, elicit, approval, "first") }()
		synctest.Wait()
		go func() { second <- approveSearch(t.Context(), elicit, approval, "second") }()
		synctest.Wait()
		cancel()
		if err := <-first; !errors.Is(err, context.Canceled) {
			t.Fatalf("owner: %v", err)
		}
		if err := <-second; err != nil || calls != 2 {
			t.Fatalf("waiter: calls=%d err=%v", calls, err)
		}
	})
}
