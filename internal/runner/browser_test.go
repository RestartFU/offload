package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserRequest(t *testing.T) {
	cases := []struct {
		hdr  map[string]string
		want bool
	}{
		{nil, false}, // Go client, curl
		{map[string]string{"Sec-Fetch-Site": "none"}, false}, // typed into the address bar
		{map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{map[string]string{"Sec-Fetch-Site": "same-origin"}, true}, // a rebound page
		{map[string]string{"Origin": "https://evil.example"}, true},
		{map[string]string{"Origin": "null"}, true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "http://100.64.0.1:7420/v1/jobs", nil)
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if got := browserRequest(r); got != c.want {
			t.Errorf("%v: got %v, want %v", c.hdr, got, c.want)
		}
	}
}

func testServer(t *testing.T, cfg *Config) *Server {
	s := &Server{cfg: cfg, whois: map[string]whoisEntry{}}
	// Pretend our own names were just looked up, so tests never ask tailscaled.
	s.ownNames.at, s.ownNames.names = time.Now(), map[string]bool{"runner": true, "runner.tail1234.ts.net": true}
	return s
}

func TestHostOK(t *testing.T) {
	s := testServer(t, &Config{Hostnames: []string{"Box.Example."}})
	for host, want := range map[string]bool{
		"100.64.0.1:7420":              true,
		"[fd7a:115c:a1e0::1]:7420":     true,
		"localhost:7420":               true,
		"runner:7420":                  true,
		"RUNNER.tail1234.ts.net.:7420": true,
		"box.example:7420":             true,
		"box.example":                  true,
		"evil.example:7420":            false, // a rebound attacker name
		"runner.evil.example:7420":     false,
		"100.64.0.1.nip.io:7420":       false,
		"":                             false,
	} {
		if got := s.hostOK(host); got != want {
			t.Errorf("hostOK(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestAuthRefusesBrowsersAndForeignHosts(t *testing.T) {
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, &Config{TokensFile: tokens})
	h := s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	do := func(host string, hdr map[string]string) (int, string) {
		r := httptest.NewRequest("POST", "http://"+host+"/v1/jobs", strings.NewReader(`{"argv":["true"]}`))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	// A token still works by any name: a page can't attach it.
	if code, body := do("box.example:7420", map[string]string{"Authorization": "Bearer secret"}); code != 204 {
		t.Fatalf("token caller: %d %s", code, body)
	}
	// A browser is refused even with a token.
	if code, body := do("100.64.0.1:7420", map[string]string{"Authorization": "Bearer secret", "Sec-Fetch-Site": "cross-site"}); code != 403 || !strings.Contains(body, "web pages") {
		t.Fatalf("browser: %d %s", code, body)
	}
	// An unknown name is refused before the tailnet identity is even looked up.
	if code, body := do("evil.example:7420", nil); code != 403 || !strings.Contains(body, "hostnames") {
		t.Fatalf("foreign host: %d %s", code, body)
	}
}
