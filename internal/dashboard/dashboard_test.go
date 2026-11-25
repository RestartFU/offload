package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	cookie := map[string]string{"Cookie": cookieName + "=k3y"}
	with := func(m map[string]string) map[string]string {
		out := map[string]string{"Cookie": cookieName + "=k3y"}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	// The printed link sets the cookie and drops the key from the address bar.
	w := do(h, "GET", "/?token=k3y", nil)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Set-Cookie"), "SameSite=Strict") || !strings.Contains(w.Header().Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("token link: %d %v", w.Code, w.Header())
	}
	if w := do(h, "GET", "/?token=nope", nil); w.Code != 403 {
		t.Errorf("wrong token: %d", w.Code)
	}
	if w := do(h, "GET", "/api/fleet", nil); w.Code != 401 {
		t.Errorf("no cookie: %d", w.Code)
	}
	if w := do(h, "GET", "/api/fleet", cookie); w.Code != 204 {
		t.Errorf("cookie: %d", w.Code)
	}
	if w := do(h, "GET", "/api/fleet", with(map[string]string{"Host": "localhost:7421"})); w.Code != 204 {
		t.Errorf("localhost: %d", w.Code)
	}
	if w := do(h, "GET", "/api/fleet", with(map[string]string{"Host": "evil.example:7421"})); w.Code != 403 {
		t.Errorf("rebound host: %d", w.Code)
	}
	// Changes need our own page: the custom header, and no cross-site markers.
	if w := do(h, "POST", "/api/stop", cookie); w.Code != 403 {
		t.Errorf("post without header: %d", w.Code)
	}
	if w := do(h, "POST", "/api/stop", with(map[string]string{"X-Offload-Dashboard": "1", "Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:7421"})); w.Code != 204 {
		t.Errorf("same-origin post: %d", w.Code)
	}
	if w := do(h, "POST", "/api/stop", with(map[string]string{"X-Offload-Dashboard": "1", "Sec-Fetch-Site": "cross-site"})); w.Code != 403 {
		t.Errorf("cross-site post: %d", w.Code)
	}
	if w := do(h, "POST", "/api/stop", with(map[string]string{"X-Offload-Dashboard": "1", "Origin": "http://evil.example"})); w.Code != 403 {
		t.Errorf("foreign origin post: %d", w.Code)
	}
	if csp := do(h, "GET", "/", cookie).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP: %q", csp)
	}
}

func TestRunRefusesNonLoopback(t *testing.T) {
	err := Run(t.Context(), Options{Addr: "0.0.0.0:0"})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("got %v", err)
	}
}
