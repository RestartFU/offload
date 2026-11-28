package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RestartFU/offload/internal/tsapi"
)

// apiLevel is what GET /api/info reports to devices, raised when a change to the dashboard's API
// would break the phone app.
const apiLevel = 1

const (
	pairCodeTTL      = 10 * time.Minute
	pairCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no 0/O or 1/I/L to misread
)

// device is a paired phone or tablet. Only the token's hash is kept.
type device struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	PairedAt time.Time `json:"paired_at"`
}

// devices holds the paired devices (devices.json) and the pairing code shown in the page.
type devices struct {
	path string

	mu       sync.Mutex
	list     []device
	seen     map[string]time.Time // last request by device ID, since the dashboard started
	code     string
	codeTill time.Time
}

func loadDevices(dir string) (*devices, error) {
	d := &devices{path: filepath.Join(dir, "devices.json"), seen: map[string]time.Time{}}
	b, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &d.list); err != nil {
		return nil, fmt.Errorf("%s is damaged (%v); delete it and pair your devices again", d.path, err)
	}
	return d, nil
}

// saveLocked writes the list atomically, readable only by this user.
func (d *devices) saveLocked() error {
	b, err := json.MarshalIndent(d.list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.path)
}

// newCode replaces the pairing code with a fresh one.
func (d *devices) newCode() (string, time.Time, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	for i := range b {
		b[i] = pairCodeAlphabet[int(b[i])%len(pairCodeAlphabet)]
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.code, d.codeTill = string(b), time.Now().Add(pairCodeTTL)
	return d.code, d.codeTill, nil
}

func (d *devices) cancelCode() {
	d.mu.Lock()
	d.code = ""
	d.mu.Unlock()
}

// normCode accepts a code as people type it: any case, with spaces or dashes.
func normCode(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, s)
}

var deviceNameRE = regexp.MustCompile(`[^\p{L}\p{N} ._'()-]+`)

// pair trades the pairing code for a new device token. A wrong code uses the code up, so it can't
// be guessed: the page then offers a new one.
func (d *devices) pair(code, name string) (token string, dev device, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.code == "" || time.Now().After(d.codeTill) {
		d.code = ""
		return "", device{}, errors.New("no pairing code is waiting: in the offload dashboard on your computer, open Devices and press Pair a phone")
	}
	ok := subtle.ConstantTimeCompare([]byte(normCode(code)), []byte(d.code)) == 1
	d.code = ""
	if !ok {
		return "", device{}, errors.New("wrong pairing code, so it is used up: press Pair a phone in the dashboard again for a new one")
	}
	tb := make([]byte, 32)
	ib := make([]byte, 6)
	if _, err := rand.Read(tb); err != nil {
		return "", device{}, err
	}
	if _, err := rand.Read(ib); err != nil {
		return "", device{}, err
	}
	token = hex.EncodeToString(tb)
	sum := sha256.Sum256([]byte(token))
	name = strings.TrimSpace(deviceNameRE.ReplaceAllString(name, ""))
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" {
		name = "Phone"
	}
	dev = device{ID: hex.EncodeToString(ib), Name: name, Hash: hex.EncodeToString(sum[:]), PairedAt: time.Now().UTC().Truncate(time.Second)}
	d.list = append(d.list, dev)
	if err := d.saveLocked(); err != nil {
		d.list = d.list[:len(d.list)-1]
		return "", device{}, err
	}
	d.seen[dev.ID] = time.Now()
	return token, dev, nil
}

// lookup finds the device a token belongs to and notes that it was seen.
func (d *devices) lookup(token string) (device, bool) {
	if token == "" {
		return device{}, false
	}
	sum := sha256.Sum256([]byte(token))
	want := hex.EncodeToString(sum[:])
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, dev := range d.list {
		if subtle.ConstantTimeCompare([]byte(dev.Hash), []byte(want)) == 1 {
			d.seen[dev.ID] = time.Now()
			return dev, true
		}
	}
	return device{}, false
}

func (d *devices) forget(id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, dev := range d.list {
		if dev.ID == id {
			old := d.list
			d.list = append(append([]device{}, old[:i]...), old[i+1:]...)
			if err := d.saveLocked(); err != nil {
				d.list = old
				return false, err
			}
			delete(d.seen, id)
			return true, nil
		}
	}
	return false, nil
}

// deviceView is a device as the page lists it.
type deviceView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	PairedAt time.Time  `json:"paired_at"`
	LastSeen *time.Time `json:"last_seen,omitempty"` // since the dashboard started
}

func (d *devices) views() []deviceView {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []deviceView{}
	for _, dev := range d.list {
		v := deviceView{ID: dev.ID, Name: dev.Name, PairedAt: dev.PairedAt}
		if t, ok := d.seen[dev.ID]; ok {
			v.LastSeen = &t
		}
		out = append(out, v)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].PairedAt.After(out[b].PairedAt) })
	return out
}

func (d *devices) pending() (string, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.code == "" || time.Now().After(d.codeTill) {
		return "", time.Time{}
	}
	return d.code, d.codeTill
}

// loadKey returns the browser key kept in path, making one the first time. It outlives the
// dashboard so a bookmarked link keeps working after a restart.
func loadKey(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		k := strings.TrimSpace(string(b))
		if len(k) == 48 {
			if _, err := hex.DecodeString(k); err == nil {
				return k, nil
			}
		}
		return "", fmt.Errorf("%s is damaged; delete it and the dashboard makes a new one (open its new link)", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return loadKey(path) // another dashboard made it first
	}
	if err != nil {
		return "", err
	}
	k := hex.EncodeToString(b)
	_, err = f.WriteString(k + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return k, err
}

// remote is the dashboard's tailnet side: the API alone, for paired devices.
type remote struct {
	addrs []string // where devices reach it, e.g. "100.101.102.103:7421"
	name  string   // this machine's tailnet name, e.g. "laptop"
	dns   string   // its full MagicDNS name, e.g. "laptop.tail1234.ts.net"
	err   string   // why devices can't reach it, when they can't
}

// listenTailnet listens on this machine's tailnet addresses, at port.
func listenTailnet(ctx context.Context, port string) ([]net.Listener, *remote) {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := tsapi.GetStatus(tctx)
	if err != nil || st.Self == nil || len(st.Self.TailscaleIPs) == 0 {
		return nil, &remote{err: "Tailscale isn't running on this computer, so phones can't reach the dashboard"}
	}
	r := &remote{name: strings.SplitN(st.Self.DNSName, ".", 2)[0], dns: strings.TrimSuffix(st.Self.DNSName, ".")}
	if r.name == "" {
		r.name = st.Self.HostName
	}
	var lns []net.Listener
	var errs []string
	for _, ip := range st.Self.TailscaleIPs {
		addr := net.JoinHostPort(ip, port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		lns = append(lns, ln)
		r.addrs = append(r.addrs, addr)
	}
	if len(lns) == 0 {
		r.err = "phones can't reach the dashboard: " + strings.Join(errs, "; ")
	}
	return lns, r
}

const deviceTokenPrefix = "Bearer "

// deviceGuard admits requests from the tailnet listener: the API only, never from a web page,
// and only with a paired device's token (but for pairing itself, which needs the code).
func (s *server) deviceGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Header.Get("Origin") != "" || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "none") {
			writeErr(w, http.StatusForbidden, "web pages can't use the dashboard from the tailnet; open it on this computer")
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/pair":
			s.handleDevicePair(w, r)
			return
		case !strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/api/devices"):
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), deviceTokenPrefix)
		dev, ok := s.devices.lookup(tok)
		if !ok {
			msg := "this device isn't paired with the dashboard any more: pair it again from Devices in the dashboard"
			if tok == "" {
				msg = "pair this device first: in the dashboard, open Devices and press Pair a phone"
			}
			writeErr(w, http.StatusUnauthorized, msg)
			return
		}
		if r.Method == "DELETE" && r.URL.Path == "/api/device" {
			if _, err := s.devices.forget(dev.ID); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			writeJSON(w, map[string]string{"status": "forgotten"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleDevicePair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, 400, "want {\"code\": \"…\", \"name\": \"…\"}")
		return
	}
	token, dev, err := s.devices.pair(req.Code, req.Name)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	fmt.Printf("offload dashboard: paired %s\n", dev.Name)
	writeJSON(w, map[string]any{"token": token, "id": dev.ID, "info": s.info()})
}

// infoView is GET /api/info: what a device shows about the computer it is paired with.
type infoView struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	APILevel int    `json:"api_level"`
}

func (s *server) info() infoView {
	name := ""
	if s.remote != nil {
		name = s.remote.name
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	return infoView{Name: name, Version: s.opts.Version, APILevel: apiLevel}
}

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.info())
}

// handleDevices lists paired devices and the pairing code waiting, for the page.
func (s *server) handleDevices(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"devices": s.devices.views()}
	if s.remote != nil {
		out["addresses"], out["name"], out["dns"] = s.remote.addrs, s.remote.name, s.remote.dns
		if s.remote.err != "" {
			out["error"] = s.remote.err
		}
	} else {
		out["error"] = "phones can't reach this dashboard: it was started with --no-remote"
	}
	if code, till := s.devices.pending(); code != "" {
		out["code"], out["code_expires"] = code, till
	}
	writeJSON(w, out)
}

func (s *server) handleDevicesPair(w http.ResponseWriter, r *http.Request) {
	if s.remote == nil || len(s.remote.addrs) == 0 {
		writeErr(w, 409, "phones can't reach this dashboard, so there is nothing to pair with")
		return
	}
	code, till, err := s.devices.newCode()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"code": code, "code_expires": till})
}

func (s *server) handleDevicesCancel(w http.ResponseWriter, r *http.Request) {
	s.devices.cancelCode()
	writeJSON(w, map[string]string{"status": "cancelled"})
}

func (s *server) handleDevicesForget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ok, err := s.devices.forget(req.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !ok {
		writeErr(w, 404, "no such device")
		return
	}
	writeJSON(w, map[string]string{"status": "forgotten"})
}
