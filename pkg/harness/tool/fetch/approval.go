package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
	"golang.org/x/net/idna"
)

// Approvals cover an origin, including its scheme and effective port. Different
// services on the same host must not inherit one another's approval.
type hostApprovals struct {
	mu       sync.Mutex
	approved map[string]bool
	pending  map[string]*hostApproval
}

type hostApproval struct {
	done chan struct{}
	err  error
}

func (h *hostApprovals) allowed(origin string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.approved[origin]
}

func approveHost(ctx context.Context, elicit *tool.Elicitation, approvals *hostApprovals, origin, displayURL string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if elicit == nil || elicit.Confirm == nil {
			return nil
		}
		approvals.mu.Lock()
		if approvals.approved[origin] {
			approvals.mu.Unlock()
			return nil
		}
		if pending := approvals.pending[origin]; pending != nil {
			approvals.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-pending.done:
				// A cancelled caller must not cancel independent callers waiting
				// for the same approval. A decline is shared by the whole group.
				if errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, context.DeadlineExceeded) {
					continue
				}
				return pending.err
			}
		}
		pending := &hostApproval{done: make(chan struct{})}
		if approvals.pending == nil {
			approvals.pending = make(map[string]*hostApproval)
		}
		approvals.pending[origin] = pending
		approvals.mu.Unlock()

		verdict, err := elicit.Approve(ctx, tool.Approval{
			Message: fmt.Sprintf("Fetch from %s?\n%s", origin, displayURL),
			Action:  map[string]any{"type": "fetch", "origin": origin, "url": displayURL, "scope": "allows every request to this origin for the rest of the session"},
		})
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if err == nil {
			err = verdict.Err("the user declined fetching from " + origin)
		}
		approvals.mu.Lock()
		if err == nil {
			if approvals.approved == nil {
				approvals.approved = make(map[string]bool)
			}
			approvals.approved[origin] = true
		}
		pending.err = err
		delete(approvals.pending, origin)
		close(pending.done)
		approvals.mu.Unlock()
		return err
	}
}

func fetchOrigin(u *url.URL) (string, error) {
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q: only http and https are allowed", u.Scheme)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || u.Opaque != "" {
		return "", fmt.Errorf("url must have an HTTP(S) host")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.String()
	} else {
		host, err = idna.Lookup.ToASCII(host)
		if err != nil || host == "" {
			return "", fmt.Errorf("invalid URL host")
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("URL port must be between 1 and 65535")
		}
		port = strconv.Itoa(n)
	}
	if port == "" {
		port = "80"
		if scheme == "https" {
			port = "443"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}
