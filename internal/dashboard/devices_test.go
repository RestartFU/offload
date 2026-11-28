package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeySurvivesRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard", "key")
	a, err := loadKey(path)
	if err != nil || len(a) != 48 {
		t.Fatalf("first key %q: %v", a, err)
	}
	b, err := loadKey(path)
	if err != nil || b != a {
		t.Fatalf("second start got %q (%v), want %q", b, err, a)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	os.WriteFile(path, []byte("short"), 0o600)
	if _, err := loadKey(path); err == nil {
		t.Fatal("a damaged key was accepted")
	}
}

func TestPairing(t *testing.T) {
	dir := t.TempDir()
	d, _ := loadDevices(dir)
	if _, _, err := d.pair("anything", "Pixel"); err == nil {
		t.Fatal("paired without a code waiting")
	}
	code, _, _ := d.newCode()
	if _, _, err := d.pair("WRONG000", "Pixel"); err == nil || !strings.Contains(err.Error(), "used up") {
		t.Fatalf("wrong code: %v", err)
	}
	if _, _, err := d.pair(code, "Pixel"); err == nil {
		t.Fatal("a code survived a wrong guess")
	}
	code, _, _ = d.newCode()
	typed := strings.ToLower(code[:4]) + "-" + code[4:]
	tok, dev, err := d.pair(typed, "Pixel <8> Pro")
	if err != nil || len(tok) != 64 || dev.Name != "Pixel 8 Pro" {
		t.Fatalf("pair: %q %+v %v", tok, dev, err)
	}
	if _, _, err := d.pair(code, "again"); err == nil {
		t.Fatal("a code was used twice")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "devices.json"))
	if strings.Contains(string(b), tok) {
		t.Fatal("the token itself is stored")
	}
	d2, err := loadDevices(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := d2.lookup(tok); !ok || got.ID != dev.ID {
		t.Fatal("the token didn't survive a restart")
	}
	if _, ok := d2.lookup(tok + "x"); ok {
		t.Fatal("a wrong token was accepted")
	}
	d2.code, d2.codeTill = "ABCDEFGH", time.Now().Add(-time.Second)
	if _, _, err := d2.pair("ABCDEFGH", "late"); err == nil {
		t.Fatal("an expired code paired")
	}
	if ok, err := d2.forget(dev.ID); !ok || err != nil {
		t.Fatalf("forget: %v %v", ok, err)
	}
	if _, ok := d2.lookup(tok); ok {
		t.Fatal("a forgotten device is still let in")
	}
}

func TestDeviceGuard(t *testing.T) {
	d, _ := loadDevices(t.TempDir())
	s := &server{token: "k3y", host: "127.0.0.1:7421", devices: d, remote: &remote{name: "laptop", addrs: []string{"100.64.0.1:7421"}}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := s.deviceGuard(ok)
	req := func(method, path, tok string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "100.64.0.1:7421"
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := req("GET", "/api/fleet", "", nil, ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := req("GET", "/api/fleet", "k3y", nil, ""); w.Code != 401 {
		t.Fatalf("the browser key works from the tailnet: %d", w.Code)
	}
	code, _, _ := d.newCode()
	if w := req("POST", "/api/pair", "", map[string]string{"Origin": "http://evil.example"}, `{"code":"`+code+`"}`); w.Code != 403 {
		t.Fatalf("pairing from a web page: %d", w.Code)
	}
	w := req("POST", "/api/pair", "", nil, `{"code":"`+code+`","name":"Pixel"}`)
	var res struct {
		Token string   `json:"token"`
		Info  infoView `json:"info"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Token == "" || res.Info.Name != "laptop" || res.Info.APILevel != apiLevel {
		t.Fatalf("pair: %d %s", w.Code, w.Body)
	}
	if w := req("GET", "/api/fleet", res.Token, nil, ""); w.Code != 204 {
		t.Fatalf("paired: %d", w.Code)
	}
	if w := req("POST", "/api/stop", res.Token, nil, "{}"); w.Code != 204 {
		t.Fatalf("paired change: %d", w.Code)
	}
	if w := req("GET", "/api/fleet", res.Token, map[string]string{"Sec-Fetch-Site": "cross-site"}, ""); w.Code != 403 {
		t.Fatalf("a web page with a token: %d", w.Code)
	}
	for _, p := range []string{"/", "/app.js", "/api/devices"} {
		if w := req("GET", p, res.Token, nil, ""); w.Code != 404 {
			t.Fatalf("%s from the tailnet: %d", p, w.Code)
		}
	}
	if w := req("POST", "/api/devices/pair", res.Token, nil, "{}"); w.Code != 404 {
		t.Fatalf("a phone made a pairing code: %d", w.Code)
	}
	if w := req("DELETE", "/api/device", res.Token, nil, ""); w.Code != 200 {
		t.Fatalf("forget self: %d", w.Code)
	}
	if w := req("GET", "/api/fleet", res.Token, nil, ""); w.Code != 401 {
		t.Fatalf("after forgetting itself: %d", w.Code)
	}
}

// The page's device routes stay behind the loopback guard and its key.
func TestDevicesNeedThePage(t *testing.T) {
	d, _ := loadDevices(t.TempDir())
	s := &server{token: "k3y", host: "127.0.0.1:7421", devices: d, remote: &remote{name: "laptop", addrs: []string{"100.64.0.1:7421"}}}
	h := s.guard(s.routes())
	if w := do(h, "POST", "/api/devices/pair", map[string]string{keyHeader: "k3y"}); w.Code != 403 {
		t.Fatalf("pairing code without the page's header: %d", w.Code)
	}
	w := do(h, "POST", "/api/devices/pair", map[string]string{keyHeader: "k3y", "X-Offload-Dashboard": "1", "Sec-Fetch-Site": "same-origin"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"code"`) {
		t.Fatalf("pairing code: %d %s", w.Code, w.Body)
	}
	if code, _ := d.pending(); code == "" {
		t.Fatal("no code waiting")
	}
}
