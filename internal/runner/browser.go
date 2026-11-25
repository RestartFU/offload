package runner

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/RestartFU/offload/internal/tsapi"
)

// Browsers must never drive the runner API. Tailnet callers are trusted by where their
// connection comes from, and a browser on an allowed machine carries that identity for every
// page it shows: any site could POST a job (cross-site request forgery), or rebind its own DNS
// name to a runner address and then read responses as same-origin (DNS rebinding). Real
// clients are Go programs, curl and the like, which send neither of the headers checked here.

// browserRequest reports whether r was made by a web page. Browsers send Origin on cross-origin
// requests and on every POST, and Sec-Fetch-Site on all requests; "none" means the user typed
// the URL (or opened a bookmark), which no page can trigger.
func browserRequest(r *http.Request) bool {
	if r.Header.Get("Origin") != "" {
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "none":
		return false
	}
	return true
}

// selfNames caches the DNS names this machine is known by (hostname, tailnet name).
type selfNames struct {
	mu    sync.Mutex
	at    time.Time
	names map[string]bool
}

// selfNamesTTL bounds how often an unknown Host triggers a fresh look at our own names.
const selfNamesTTL = time.Minute

// normHost lowercases a Host header value and strips its port, brackets and trailing dot.
func normHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
}

// hostOK reports whether a request's Host names this runner. DNS rebinding needs an attacker's
// name in Host, so IP addresses, localhost, our own host and tailnet names and the configured
// hostnames pass; anything else is refused for callers trusted by tailnet identity.
func (s *Server) hostOK(host string) bool {
	h := normHost(host)
	if h == "" {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil || h == "localhost" {
		return true
	}
	for _, n := range s.cfg.Hostnames {
		if normHost(n) == h {
			return true
		}
	}
	return s.ownNames.has(h)
}

func (n *selfNames) has(h string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.names[h] {
		return true
	}
	if time.Since(n.at) < selfNamesTTL {
		return false
	}
	n.at, n.names = time.Now(), lookupSelfNames()
	return n.names[h]
}

// lookupSelfNames returns this machine's hostname and tailnet names, full and short.
func lookupSelfNames() map[string]bool {
	out := map[string]bool{}
	add := func(name string) {
		name = normHost(name)
		if name == "" {
			return
		}
		out[name] = true
		out[strings.Split(name, ".")[0]] = true
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if st, err := tsapi.GetStatus(ctx); err == nil && st.Self != nil {
		add(st.Self.DNSName)
		add(st.Self.HostName)
	}
	return out
}
