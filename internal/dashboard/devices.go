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
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/tsapi"
)

// apiLevel is what GET /api/info reports to devices, raised when a change to the dashboard's API
// would break the phone app.
const apiLevel = 1

const (
	pairCodeTTL      = 10 * time.Minute
	pairCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789" // no 0/O or 1/I/L to misread
	// Wrong codes one peer may send before it has to wait for the next code, and how many the
	// code survives in all. One peer can't use the code up for the phone, and 31^8 codes leave
	// no chance to a guesser with five tries.
	pairTriesPerPeer = 5
	pairTriesTotal   = 40
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
	seen     map[string]time.Time     // last request by device ID, since the dashboard started
	gone     map[string]chan struct{} // closed when that device is forgotten: ends its open requests
	code     string
	codeTill time.Time
	fails    map[string]int // wrong codes by peer address, for the code waiting
	failed   int
}

func loadDevices(dir string) (*devices, error) {
	d := &devices{path: filepath.Join(dir, "devices.json"), seen: map[string]time.Time{}, gone: map[string]chan struct{}{}}
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
	code := make([]byte, 0, 8)
	buf := make([]byte, 16)
	limit := byte(256 - 256%len(pairCodeAlphabet)) // bytes past it would favour the first letters
	for len(code) < cap(code) {
		if _, err := rand.Read(buf); err != nil {
			return "", time.Time{}, err
		}
		for _, b := range buf {
			if b < limit && len(code) < cap(code) {
				code = append(code, pairCodeAlphabet[int(b)%len(pairCodeAlphabet)])
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.code, d.codeTill = string(code), time.Now().Add(pairCodeTTL)
	d.fails, d.failed = map[string]int{}, 0
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

// pair trades the pairing code for a new device token. peer is the caller's address: its wrong
// codes are counted against it, so a job guessing on another runner can neither find the code
// nor use it up before the phone gets to it.
func (d *devices) pair(code, name, peer string) (token string, dev device, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	const again = "in the offload dashboard on your computer, open Devices and press Pair a phone"
	if d.code == "" || time.Now().After(d.codeTill) {
		d.code = ""
		return "", device{}, errors.New("no pairing code is waiting: " + again)
	}
	if d.fails[peer] >= pairTriesPerPeer {
		return "", device{}, errors.New("too many wrong codes from this device: " + again + " for a new one")
	}
	if subtle.ConstantTimeCompare([]byte(normCode(code)), []byte(d.code)) != 1 {
		d.fails[peer]++
		d.failed++
		if d.failed >= pairTriesTotal {
			d.code = ""
		}
		if left := pairTriesPerPeer - d.fails[peer]; left > 0 && d.code != "" {
			return "", device{}, fmt.Errorf("wrong pairing code: check it against the dashboard and try again (%d more before it needs a new one)", left)
		}
		return "", device{}, errors.New("wrong pairing code too many times: " + again + " for a new one")
	}
	d.code = ""
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

// lookup finds the device a token belongs to and notes that it was seen. gone is closed when
// the device is forgotten.
func (d *devices) lookup(token string) (dev device, gone <-chan struct{}, ok bool) {
	if token == "" {
		return device{}, nil, false
	}
	sum := sha256.Sum256([]byte(token))
	want := hex.EncodeToString(sum[:])
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, dev := range d.list {
		if subtle.ConstantTimeCompare([]byte(dev.Hash), []byte(want)) == 1 {
			d.seen[dev.ID] = time.Now()
			ch := d.gone[dev.ID]
			if ch == nil {
				ch = make(chan struct{})
				d.gone[dev.ID] = ch
			}
			return dev, ch, true
		}
	}
	return device{}, nil, false
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
			if ch := d.gone[id]; ch != nil {
				close(ch)
				delete(d.gone, id)
			}
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

// tryLock takes an exclusive lock on path without waiting. The open file holds it until the
// process ends.
func tryLock(path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	lockFiles = append(lockFiles, f)
	return true, nil
}

var lockFiles []*os.File // kept open: a collected file would drop its lock

// remote is the dashboard's tailnet side: the API alone, for paired devices.
type remote struct {
	mu    sync.Mutex
	addrs []string // where devices reach it, e.g. "100.101.102.103:7421"
	name  string   // this machine's tailnet name, e.g. "laptop"
	dns   string   // its full MagicDNS name, e.g. "laptop.tail1234.ts.net"
	err   string   // why devices can't reach it, when they can't
}

func (r *remote) get() (addrs []string, name, dns, err string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addrs, r.name, r.dns, r.err
}

func (r *remote) fail(msg string) {
	r.mu.Lock()
	r.addrs, r.err = nil, msg
	r.mu.Unlock()
}

// listen binds this machine's tailnet addresses at port and records where devices reach it,
// or why they can't.
func (r *remote) listen(ctx context.Context, port string) []net.Listener {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := tsapi.GetStatus(tctx)
	if err != nil || st.Self == nil || len(st.Self.TailscaleIPs) == 0 {
		r.fail("Tailscale isn't running on this computer, so phones can't reach the dashboard; it starts serving them once Tailscale is up")
		return nil
	}
	var lns []net.Listener
	var addrs, errs []string
	for _, ip := range st.Self.TailscaleIPs {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, port))
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		lns = append(lns, ln)
		addrs = append(addrs, ln.Addr().String())
	}
	if len(lns) == 0 {
		r.fail("phones can't reach the dashboard: " + strings.Join(errs, "; "))
		return nil
	}
	name := strings.SplitN(st.Self.DNSName, ".", 2)[0]
	if name == "" {
		name = st.Self.HostName
	}
	r.mu.Lock()
	r.addrs, r.name, r.dns, r.err = addrs, name, strings.TrimSuffix(st.Self.DNSName, "."), ""
	r.mu.Unlock()
	return lns
}

// serveTailnet serves paired devices on the tailnet, at the port the page has. A dashboard
// started before Tailscale (a user service at login) keeps trying until the addresses exist.
func (s *server) serveTailnet(ctx context.Context, port string, dev *http.Server) {
	said := ""
	for {
		if lns := s.remote.listen(ctx, port); len(lns) > 0 {
			addrs, _, _, _ := s.remote.get()
			fmt.Printf("phones paired under Devices reach the dashboard at %s\n", strings.Join(addrs, ", "))
			for _, l := range lns {
				go func() {
					if err := dev.Serve(l); !errors.Is(err, http.ErrServerClosed) {
						s.remote.fail(fmt.Sprintf("phones can't reach the dashboard any more (%v): restart it", err))
						fmt.Printf("phones can't reach the dashboard at %s any more: %v\n", l.Addr(), err)
					}
				}()
			}
			return
		}
		if _, _, _, why := s.remote.get(); why != said {
			fmt.Println(why)
			said = why
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
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
		dev, gone, ok := s.devices.lookup(tok)
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
		// Forgetting the device ends what it has open, such as a log it is following.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			select {
			case <-gone:
				cancel()
			case <-ctx.Done():
			}
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
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
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	token, dev, err := s.devices.pair(req.Code, req.Name, peer)
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
	_, name, _, _ := s.remote.get()
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
	addrs, name, dns, why := s.remote.get()
	out := map[string]any{"devices": []deviceView{}}
	if why != "" {
		out["error"] = why
	}
	if len(addrs) > 0 {
		out["addresses"], out["name"], out["dns"] = addrs, name, dns
	}
	if s.devices != nil {
		out["devices"] = s.devices.views()
		if code, till := s.devices.pending(); code != "" && len(addrs) > 0 {
			out["code"], out["code_expires"] = code, till
		}
	}
	writeJSON(w, out)
}

func (s *server) handleDevicesPair(w http.ResponseWriter, r *http.Request) {
	if addrs, _, _, _ := s.remote.get(); s.devices == nil || len(addrs) == 0 {
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
	if s.devices != nil {
		s.devices.cancelCode()
	}
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
	if s.devices == nil {
		_, _, _, why := s.remote.get()
		writeErr(w, 409, why)
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
