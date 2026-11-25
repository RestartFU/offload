package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RestartFU/offload/internal/api"
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
		r.Header.Set(api.ProtoHeader, fmt.Sprint(api.Proto))
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
	// A client of another protocol version is refused, even with a token.
	if code, body := do("box.example:7420", map[string]string{"Authorization": "Bearer secret", api.ProtoHeader: "4"}); code != 412 || !strings.Contains(body, "protocol") {
		t.Fatalf("old client: %d %s", code, body)
	}
	// Without a token, a caller is not admitted to the API (tailnet identity is only for status
	// and first pairing; the test request comes from no tailnet peer at all).
	if code, _ := do("100.64.0.1:7420", nil); code != 403 {
		t.Fatalf("tokenless caller: %d", code)
	}
	// An unknown name is refused before the tailnet identity is even looked up.
	if code, body := do("evil.example:7420", nil); code != 403 || !strings.Contains(body, "hostnames") {
		t.Fatalf("foreign host: %d %s", code, body)
	}
}

func TestPairCodeIsOneUse(t *testing.T) {
	tokens := filepath.Join(t.TempDir(), "tokens")
	s := testServer(t, &Config{TokensFile: tokens})
	code, err := NewPairCode(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.consumePairCode(""); err == nil {
		t.Fatal("empty code accepted")
	}
	if err := s.consumePairCode(code); err != nil {
		t.Fatalf("right code refused: %v", err)
	}
	if err := s.consumePairCode(code); err == nil {
		t.Fatal("code accepted twice")
	}
	code, _ = NewPairCode(tokens)
	if err := s.consumePairCode("wrong"); err == nil {
		t.Fatal("wrong code accepted")
	}
	if err := s.consumePairCode(code); err == nil {
		t.Fatal("a wrong guess must burn the code")
	}
}

func TestRemoveToken(t *testing.T) {
	tokens := filepath.Join(t.TempDir(), "tokens")
	keep := strings.Repeat("a", 64)
	if err := os.WriteFile(tokens, []byte("legacy-secret\nsha256:"+keep+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, &Config{TokensFile: tokens})
	if !s.tokenOK("legacy-secret") {
		t.Fatal("legacy token not accepted")
	}
	sum := sha256.Sum256([]byte("legacy-secret"))
	if err := s.removeToken(hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if s.tokenOK("legacy-secret") {
		t.Fatal("removed token still accepted")
	}
	if got := s.pairedTokens(); len(got) != 1 || got[0] != "sha256:"+keep {
		t.Fatalf("other tokens changed: %v", got)
	}
}
