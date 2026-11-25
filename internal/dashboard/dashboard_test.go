package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/client"
)

func testServer() (*server, http.Handler) {
	s := &server{token: "k3y", host: "127.0.0.1:7421"}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	return s, s.guard(ok)
}

func do(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader("{}"))
	r.Host = "127.0.0.1:7421"
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGuard(t *testing.T) {
	_, h := testServer()
	key := map[string]string{keyHeader: "k3y"}
	with := func(m map[string]string) map[string]string {
		out := map[string]string{keyHeader: "k3y"}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	// The page holds no secrets: served without the key, but never a cookie.
	w := do(h, "GET", "/", nil)
	if w.Code != 204 || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("page: %d %v", w.Code, w.Header())
	}
	if w := do(h, "POST", "/", key); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("post to the page: %d", w.Code)
	}
	if w := do(h, "GET", "/", map[string]string{"Host": "evil.example:7421"}); w.Code != 403 {
		t.Errorf("rebound host, page: %d", w.Code)
	}
	// The API needs the key, in the header only.
	if w := do(h, "GET", "/api/fleet", nil); w.Code != 401 {
		t.Errorf("no key: %d", w.Code)
	}
	if w := do(h, "GET", "/api/fleet", map[string]string{keyHeader: "nope"}); w.Code != 401 {
		t.Errorf("wrong key: %d", w.Code)
	}
	if w := do(h, "GET", "/api/fleet?token=k3y", map[string]string{"Cookie": "offload_dashboard=k3y"}); w.Code != 401 {
		t.Errorf("key outside the header: %d", w.Code)
	}
	if w := do(h, "GET", "/api/fleet", key); w.Code != 204 {
		t.Errorf("key: %d", w.Code)
	}
	if w := do(h, "GET", "/api/fleet", with(map[string]string{"Host": "localhost:7421"})); w.Code != 204 {
		t.Errorf("localhost: %d", w.Code)
	}
	if w := do(h, "GET", "/api/fleet", with(map[string]string{"Host": "evil.example:7421"})); w.Code != 403 {
		t.Errorf("rebound host: %d", w.Code)
	}
	// Changes need our own page: the custom header, and no cross-site markers.
	if w := do(h, "POST", "/api/stop", key); w.Code != 403 {
		t.Errorf("post without header: %d", w.Code)
	}
	if w := do(h, "POST", "/api/stop", with(map[string]string{"X-Offload-Dashboard": "1", "Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:7421"})); w.Code != 204 {
		t.Errorf("same-origin post: %d", w.Code)
	}
	if w := do(h, "POST", "/api/stop", with(map[string]string{"X-Offload-Dashboard": "1", "Host": "localhost:7421", "Sec-Fetch-Site": "same-origin", "Origin": "http://localhost:7421"})); w.Code != 204 {
		t.Errorf("same-origin post via localhost: %d", w.Code)
	}
	if w := do(h, "POST", "/api/stop", with(map[string]string{"X-Offload-Dashboard": "1", "Sec-Fetch-Site": "cross-site"})); w.Code != 403 {
		t.Errorf("cross-site post: %d", w.Code)
	}
	if w := do(h, "POST", "/api/stop", with(map[string]string{"X-Offload-Dashboard": "1", "Origin": "http://evil.example"})); w.Code != 403 {
		t.Errorf("foreign origin post: %d", w.Code)
	}
	if csp := do(h, "GET", "/", nil).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP: %q", csp)
	}
}

func TestFleetNamesStayUnique(t *testing.T) {
	f := newFleet(nil)
	f.addLocked(&client.Host{Name: "build", URL: "http://100.64.0.1:7422"})
	f.addLocked(&client.Host{Name: "build", URL: "http://100.64.0.2:7422"})
	f.addLocked(&client.Host{Name: "build", URL: "http://100.64.0.1:7422"}) // rediscovered: same row
	v := f.snapshot("")
	if len(v.Runners) != 2 || v.Runners[0].Name != "build" || v.Runners[1].Name != "build (100.64.0.2:7422)" {
		t.Fatalf("runners: %+v", v.Runners)
	}
	if h := f.host("build (100.64.0.2:7422)"); h == nil || h.URL != "http://100.64.0.2:7422" {
		t.Fatalf("lookup: %+v", h)
	}
	// A runner whose own name is what the second one was given: still unique.
	f = newFleet(nil)
	f.addLocked(&client.Host{Name: "build", URL: "http://100.64.0.1:7422"})
	f.addLocked(&client.Host{Name: "build (100.64.0.2:7422)", URL: "http://100.64.0.3:7422"})
	f.addLocked(&client.Host{Name: "build", URL: "http://100.64.0.2:7422"})
	seen := map[string]string{}
	for _, r := range f.snapshot("").Runners {
		if prev, dup := seen[r.Name]; dup {
			t.Fatalf("%q names both %s and %s", r.Name, prev, r.URL)
		}
		seen[r.Name] = r.URL
		if h := f.host(r.Name); h == nil || h.URL != r.URL {
			t.Fatalf("%q resolves to %+v, want %s", r.Name, h, r.URL)
		}
	}
}

func TestOpenBrowserKeepsKeyOffTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	bin := t.TempDir()
	// A stand-in for xdg-open/open that records its arguments.
	script := "#!/bin/sh\necho \"$@\" > " + filepath.Join(dir, "args") + "\n"
	for _, name := range []string{"xdg-open", "open"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	link := "http://127.0.0.1:7421/#key=s3cret"
	if err := openBrowser(t.Context(), link); err != nil {
		t.Fatal(err)
	}
	var args []byte
	for i := 0; i < 100 && len(args) == 0; i++ {
		args, _ = os.ReadFile(filepath.Join(dir, "args"))
		time.Sleep(10 * time.Millisecond)
	}
	if len(args) == 0 || strings.Contains(string(args), "s3cret") {
		t.Fatalf("opener got %q", args)
	}
	page := strings.TrimSpace(string(args))
	st, err := os.Stat(page)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("redirect file %q: %v %v", page, st, err)
	}
	b, _ := os.ReadFile(page)
	if !strings.Contains(string(b), "url="+link) {
		t.Fatalf("redirect page: %s", b)
	}
}

func TestRunRefusesNonLoopback(t *testing.T) {
	err := Run(t.Context(), Options{Addr: "0.0.0.0:0"})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("got %v", err)
	}
}

func TestFleetOneRowPerRunnerID(t *testing.T) {
	f := newFleet(nil)
	// Seen first without an ID (an older runner), then upgraded: the row moves to its ID.
	f.addLocked(&client.Host{Name: "build", URL: "http://build.example:7420", Status: &api.Status{}})
	f.addLocked(&client.Host{Name: "build", URL: "http://build.example:7420", Status: &api.Status{ID: "r1"}})
	// The same runner found again at its tailnet address: not a second row.
	f.addLocked(&client.Host{Name: "build", URL: "http://100.64.0.1:7420", Status: &api.Status{ID: "r1"}})
	v := f.snapshot("")
	if len(v.Runners) != 1 || v.Runners[0].URL != "http://build.example:7420" || v.Runners[0].Name != "build" {
		t.Fatalf("runners: %+v", v.Runners)
	}
	// The other address is kept to fall back on, once.
	f.addLocked(&client.Host{Name: "build", URL: "http://100.64.0.1:7420", Status: &api.Status{ID: "r1"}})
	if r := f.runners["r1"]; len(r.alts) != 1 || r.alts[0].URL != "http://100.64.0.1:7420" {
		t.Fatalf("alternates: %+v", r.alts)
	}
}
