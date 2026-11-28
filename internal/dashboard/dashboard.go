// Package dashboard serves `offload dashboard`: a local web page showing every runner, its
// cores and memory, the jobs and sessions on it, and statistics from its ledger. It runs on
// the laptop, talks to runners with the client's own identity, and listens on loopback only.
package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/client"
)

//go:embed ui
var uiFiles embed.FS

// Options configures the dashboard.
type Options struct {
	Addr    string // loopback address to listen on, e.g. "127.0.0.1:7421"
	Open    bool   // open the browser
	Version string // this binary's version, compared with the runners'
}

// keyHeader carries the per-run key on every API request. The page keeps the key in its own
// origin's storage (scoped to this port), not in a cookie: cookies are shared by every port on
// 127.0.0.1, so any other local web server the browser visits would receive one.
const keyHeader = "X-Offload-Key"

type server struct {
	opts  Options
	token string
	host  string // the Host header the page is served under, e.g. "127.0.0.1:7421"
	fleet *fleet
}

// Run serves the dashboard until ctx ends.
func Run(ctx context.Context, opts Options) error {
	cfg, err := client.LoadConfig()
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("the dashboard listens on loopback only (got %s): it acts with this machine's identity", host)
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil && port != "0" {
		ln, err = net.Listen("tcp", net.JoinHostPort(host, "0")) // the usual port is taken: any port
	}
	if err != nil {
		return err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	s := &server{opts: opts, token: hex.EncodeToString(b), host: ln.Addr().String(), fleet: newFleet(cfg)}
	go s.fleet.run(ctx)

	// The key travels in the fragment: browsers don't send it to the server or keep it in
	// Referer. The page moves it to its own origin's storage and sends it with each API call.
	link := "http://" + s.host + "/#key=" + s.token
	fmt.Printf("offload dashboard: %s\n(the link holds this session's key; Ctrl-C stops the dashboard)\n", link)
	if opts.Open {
		if err := openBrowser(ctx, link); err != nil {
			fmt.Printf("could not open a browser (%v); open the link above\n", err)
		}
	}
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// openBrowser opens link without putting it on a command line, where other local users could
// read the key (ps, /proc/PID/cmdline): it writes a private HTML file that redirects to link
// and opens that file instead. The file is removed after a minute, or when the dashboard stops.
func openBrowser(ctx context.Context, link string) error {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, "offload-dashboard-*.html") // mode 0600
	if err != nil {
		return err
	}
	esc := html.EscapeString(link)
	_, err = fmt.Fprintf(f, "<!doctype html><meta charset=utf-8><meta name=referrer content=no-referrer>"+
		"<meta http-equiv=refresh content=\"0;url=%s\"><title>offload dashboard</title><a href=\"%s\">Open the offload dashboard</a>\n", esc, esc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(time.Minute):
		}
		os.Remove(f.Name())
	}()
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	c := exec.Command(cmd, f.Name())
	if err := c.Start(); err != nil {
		os.Remove(f.Name())
		return err
	}
	go func() {
		if err := c.Wait(); err != nil {
			fmt.Printf("%s failed (%v); open the link above\n", cmd, err)
		}
	}()
	return nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(uiFiles, "ui")
	files := http.FileServerFS(static)
	mux.Handle("GET /", files)
	mux.HandleFunc("GET /api/fleet", s.handleFleet)
	mux.HandleFunc("GET /api/series", s.handleSeries)
	mux.HandleFunc("GET /api/ledger", s.handleLedger)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("POST /api/stop", s.handleStop)
	mux.HandleFunc("POST /api/pause", s.handlePause)
	mux.HandleFunc("POST /api/github/add", s.handleGitHubAdd)
	mux.HandleFunc("POST /api/github/remove", s.handleGitHubRemove)
	return s.guard(mux)
}

// guard keeps the dashboard to this browser session. It acts with this machine's tailnet
// identity, so: the Host must be our own loopback address (no DNS rebinding), every API request
// needs the per-run key (another local user can't use it, and a cross-site page can neither
// read it nor attach the header without a CORS preflight, which we never grant), and anything
// that changes state must come from our own page. The page itself (HTML, JS, CSS) holds no
// secrets and is served to anyone who passes the Host check.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if !s.hostOK(r.Host) {
			http.Error(w, "unexpected Host", http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !s.tokenOK(r.Header.Get(keyHeader)) {
			writeErr(w, http.StatusUnauthorized, "open the link offload dashboard printed (it carries this session's key)")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			site := r.Header.Get("Sec-Fetch-Site")
			origin := r.Header.Get("Origin")
			if r.Header.Get("X-Offload-Dashboard") != "1" || (site != "" && site != "same-origin") || (origin != "" && origin != "http://"+r.Host) {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) hostOK(h string) bool {
	_, port, _ := net.SplitHostPort(s.host)
	return h == s.host || h == "localhost:"+port
}

func (s *server) tokenOK(t string) bool {
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(api.Error{Error: msg})
}

func (s *server) handleFleet(w http.ResponseWriter, r *http.Request) {
	s.fleet.watched()
	writeJSON(w, s.fleet.snapshot(s.opts.Version))
}

var idRE = regexp.MustCompile(`^[a-z]-[0-9a-f]{1,32}$`)

func (s *server) handleSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h := s.fleet.host(q.Get("host"))
	if h == nil {
		writeErr(w, 404, "no such runner")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var out api.Series
	if err := h.Do(ctx, "GET", "/v1/metrics/series?range="+url.QueryEscape(q.Get("range")), nil, &out); err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, out)
}

// ledgerRow is a ledger entry with the runner it came from.
type ledgerRow struct {
	Runner string `json:"runner"`
	api.LedgerEntry
}

// handleLedger merges the ledgers of every online runner that keeps one.
func (s *server) handleLedger(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	if _, err := time.Parse(time.RFC3339, since); err != nil {
		writeErr(w, 400, "since: want RFC 3339")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	type result struct {
		name      string
		rows      []api.LedgerEntry
		truncated bool
		err       error
	}
	hosts, missing := s.fleet.ledgerHosts()
	ch := make(chan result, len(hosts))
	for name, h := range hosts {
		go func() {
			res := result{name: name}
			resp, err := h.Request(ctx, "GET", "/v1/ledger?since="+url.QueryEscape(since), nil)
			if err == nil {
				res.truncated = resp.Header.Get("X-Offload-Truncated") != ""
				err = json.NewDecoder(resp.Body).Decode(&res.rows)
				resp.Body.Close()
			}
			res.err = err
			ch <- res
		}()
	}
	out := struct {
		Rows      []ledgerRow       `json:"rows"`
		Errors    map[string]string `json:"errors,omitempty"`
		Truncated []string          `json:"truncated,omitempty"` // runners that sent only their newest entries
	}{Rows: []ledgerRow{}}
	if len(missing) > 0 {
		out.Errors = missing
	}
	for range hosts {
		res := <-ch
		if res.err != nil {
			if out.Errors == nil {
				out.Errors = map[string]string{}
			}
			out.Errors[res.name] = res.err.Error()
			continue
		}
		if res.truncated {
			out.Truncated = append(out.Truncated, res.name)
		}
		for _, e := range res.rows {
			out.Rows = append(out.Rows, ledgerRow{Runner: res.name, LedgerEntry: e})
		}
	}
	writeJSON(w, out)
}

// handleLogs streams a job's or session's output from the runner, following it. Session
// streams are observe-only: watching doesn't keep an idle session alive. The body starts with
// one JSON line, {"offset":N} (how many earlier bytes were skipped), then the raw output. The
// line also gets a quiet stream going: browsers wait for a first byte before handing it over.
func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("host")
	h, current := s.fleet.runner(name)
	id := q.Get("id")
	if h == nil || !idRE.MatchString(id) {
		writeErr(w, 404, "no such runner or workload")
		return
	}
	if !current {
		// An older runner would read a log stream as an attached client (keeping an idle
		// session alive while the page is open) and ignore tail (sending the whole log).
		writeErr(w, 409, "this runner is too old to show logs here; upgrade it with offload host upgrade")
		return
	}
	tail, _ := strconv.ParseInt(q.Get("tail"), 10, 64)
	tail = min(max(tail, 4<<10), 1<<20)
	var base, state string // the stream without offset or follow, and where to read its state
	switch q.Get("kind") {
	case "job":
		stream := "stdout"
		if q.Get("stream") == "stderr" {
			stream = "stderr"
		}
		base, state = "/v1/jobs/"+id+"/logs?stream="+stream, "/v1/jobs/"+id
	case "session":
		stream := "stderr"
		if q.Get("stream") == "stdout" {
			stream = "stdout"
		}
		base, state = "/v1/sessions/"+id+"/observe/"+stream+"?", "/v1/sessions/"+id
	default:
		writeErr(w, 400, "kind: job or session")
		return
	}
	ctx := r.Context()
	resp, done, err := openStream(ctx, h, base+"&follow=1&tail="+strconv.FormatInt(tail, 10))
	if err != nil {
		var ae *client.APIError
		if errors.As(err, &ae) && ae.Code == 404 {
			writeErr(w, 404, ae.Msg)
			return
		}
		writeErr(w, 502, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	off, _ := strconv.ParseInt(resp.Header.Get("X-Offload-Offset"), 10, 64)
	fmt.Fprintf(w, "{\"offset\":%d}\n", off)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	// The runner ends a follow every few minutes (each reconnect is authorized again), and a
	// connection can drop: reconnect from where it stopped until the workload is over, then read
	// the rest. A drop that brought no output counts as a failure and waits before retrying.
	// Give up, saying why, on an answer retrying can't change or after about five minutes.
	follow, failures, opened := true, 0, time.Now()
	for {
		n, readErr, writeErr := copyFlush(w, resp.Body)
		resp.Body.Close()
		done()
		off += n
		if writeErr != nil || ctx.Err() != nil {
			return // the viewer went away
		}
		if readErr == nil && !follow {
			return // read to the end of a finished workload
		}
		// A quiet follow ends empty when the runner rolls it over (every five minutes); one that
		// ends empty well before that, or with an error, is a failure.
		if n > 0 {
			failures = 0
		} else if readErr != nil || (follow && time.Since(opened) < 4*time.Minute) {
			failures++
		}
		for {
			if failures > 60 {
				fmt.Fprintf(w, "\n[offload dashboard] stopped following this log: the runner keeps dropping the connection\n")
				return
			}
			if failures > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
			if h2, _ := s.fleet.runner(name); h2 != nil {
				h = h2 // the fleet may have moved to another address of this runner
			}
			var st struct {
				State string `json:"state"`
			}
			sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := h.Do(sctx, "GET", state, nil, &st)
			cancel()
			if err == nil {
				follow = !api.Terminal(st.State)
				path := base + "&offset=" + strconv.FormatInt(off, 10)
				if follow {
					path += "&follow=1"
				}
				if resp, done, err = openStream(ctx, h, path); err == nil {
					opened = time.Now()
					break
				}
			}
			if ctx.Err() != nil {
				return
			}
			var ae *client.APIError
			if errors.As(err, &ae) && ae.Code < 500 {
				fmt.Fprintf(w, "\n[offload dashboard] stopped following this log: %v\n", err)
				return
			}
			failures++
		}
	}
}

// openStream starts a streaming request that must answer within 15 seconds. The body is then
// cut off after six minutes, past the runner's five-minute follow, so a connection that went
// silent can't hold the viewer forever. done releases the request once its body is closed.
func openStream(ctx context.Context, h *client.Host, path string) (*http.Response, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(15*time.Second, cancel)
	resp, err := h.Request(ctx, "GET", path, nil)
	if !timer.Stop() && err == nil { // it fired just as the answer came: the body is cut off
		resp.Body.Close()
		err = context.DeadlineExceeded
	}
	if err != nil {
		cancel()
		return nil, nil, err
	}
	timer = time.AfterFunc(6*time.Minute, cancel)
	return resp, func() { timer.Stop(); cancel() }, nil
}

// copyFlush copies src to w, flushing after every read so output shows as it arrives. Each
// write must finish within a minute: a viewer that stops reading doesn't hold the proxy.
// readErr is nil at the end of src.
func copyFlush(w http.ResponseWriter, src io.Reader) (n int64, readErr, writeErr error) {
	rc := http.NewResponseController(w)
	defer rc.SetWriteDeadline(time.Time{}) // the connection may serve other requests after this
	buf := make([]byte, 32<<10)
	for {
		k, err := src.Read(buf)
		if k > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(time.Minute))
			if _, werr := w.Write(buf[:k]); werr != nil {
				return n, nil, werr
			}
			n += int64(k)
			if werr := rc.Flush(); werr != nil {
				return n, nil, werr
			}
		}
		if errors.Is(err, io.EOF) {
			return n, nil, nil
		} else if err != nil {
			return n, err, nil
		}
	}
}

// handleStop cancels a job or stops a session.
func (s *server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host string `json:"host"`
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	h := s.fleet.host(req.Host)
	if h == nil || !idRE.MatchString(req.ID) || (req.Kind != "job" && req.Kind != "session") {
		writeErr(w, 404, "no such runner or workload")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := h.Do(ctx, "DELETE", "/v1/"+req.Kind+"s/"+req.ID, nil, nil); err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	s.fleet.poke()
	writeJSON(w, map[string]string{"status": "stopping"})
}

// handlePause pauses or resumes a runner.
func (s *server) handlePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host   string `json:"host"`
		Paused bool   `json:"paused"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	h := s.fleet.host(req.Host)
	if h == nil {
		writeErr(w, 404, "no such runner")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var st api.Status
	if err := h.Do(ctx, "POST", "/v1/admin/pause", map[string]bool{"paused": req.Paused}, &st); err != nil {
		var ae *client.APIError
		if errors.As(err, &ae) && ae.Code == 404 {
			writeErr(w, 409, "this runner is too old to pause; upgrade it with offload host upgrade")
			return
		}
		writeErr(w, 502, err.Error())
		return
	}
	s.fleet.setStatus(req.Host, h, &st)
	writeJSON(w, st)
}

type githubResult struct {
	Runner string `json:"runner"`
	Error  string `json:"error,omitempty"`
}

// handleGitHubAdd connects an organization or repository on runners, as `offload github add`.
// A token from the page is used exactly as typed: never as a cmd:, env: or file: source.
func (s *server) handleGitHubAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner  string   `json:"owner"`
		Token  string   `json:"token"` // empty: this machine's gh login
		Labels []string `json:"labels"`
		System *bool    `json:"system_labels"` // false: the labels are all the runners get
		Max    *int     `json:"max"`
		Shared *bool    `json:"shared"`
		On     []string `json:"on"` // runner IDs, as the page chose them
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	owner, _, err := api.GitHubOwner(req.Owner)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(req.On) == 0 {
		writeErr(w, 400, "pick at least one runner")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	token, from := strings.TrimSpace(req.Token), "token"
	if token == "" {
		if token, err = client.GitHubLoginToken(ctx, owner); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		from = "gh"
	}
	if strings.ContainsAny(token, " \t\r\n") {
		writeErr(w, 400, "that doesn't look like a GitHub token")
		return
	}
	// The runners the page showed, by ID: the token goes to no other runner, not even a new one
	// under the same name.
	rows, missing := s.fleet.hostsByID(req.On)
	if len(missing) > 0 {
		writeErr(w, 409, "a runner you chose changed or stopped answering; pick the runners again")
		return
	}
	hosts, err := client.GitHubTargets(rows, nil)
	if err != nil || len(hosts) != len(rows) {
		ok := map[*client.Host]bool{}
		for _, h := range hosts {
			ok[h] = true
		}
		var bad []string
		for _, h := range rows {
			if !ok[h] {
				bad = append(bad, h.Name)
			}
		}
		writeErr(w, 400, strings.Join(bad, ", ")+" can't take GitHub jobs (not paired with this machine, no podman, or another version)")
		return
	}
	add := api.GitHubAdd{URL: owner, Token: token, TokenFrom: from, Labels: req.Labels, SystemLabels: req.System, Max: req.Max, SharedCache: req.Shared, CreateOnly: true}
	results := make([]githubResult, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].Runner = h.Name
			if _, err := client.GitHubAddTo(ctx, h, add); err != nil {
				results[i].Error = err.Error()
			}
		}()
	}
	wg.Wait()
	s.fleet.poke()
	writeJSON(w, map[string]any{"results": results})
}

// handleGitHubRemove stops taking an owner's GitHub jobs on every runner that does (forget: only
// here, when GitHub can't be told).
func (s *server) handleGitHubRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owner  string   `json:"owner"`
		Forget bool     `json:"forget"`
		On     []string `json:"on"` // runner IDs; empty: every one that listed the pool
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	owner, _, err := api.GitHubOwner(req.Owner)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ids := req.On
	if len(ids) == 0 {
		ids = s.fleet.githubHolderIDs(owner)
	}
	if len(ids) == 0 {
		writeErr(w, 404, "no runner takes GitHub jobs for "+owner)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// Every runner asked for gets a result: one that can't be checked is reported, never skipped.
	results := make([]githubResult, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		h, why := s.fleet.githubHolder(id, owner)
		if h == nil {
			results[i] = githubResult{Runner: id, Error: why}
			continue
		}
		results[i].Runner = h.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.GitHubRemoveFrom(ctx, h, owner, req.Forget); err != nil {
				results[i].Error = err.Error()
			}
		}()
	}
	wg.Wait()
	s.fleet.poke()
	writeJSON(w, map[string]any{"results": results})
}
