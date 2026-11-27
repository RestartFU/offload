package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

// MCP starts a session from a runner profile and bridges this process's stdio to it, so an MCP
// client can use `offload mcp <profile>` as a stdio server. Stdin lines are sent in order with
// sequence numbers (safe to retry); stdout resumes from the last byte after reconnects.
// MCPOptions is `offload mcp`.
type MCPOptions struct {
	Profile, On, Environment string
	Resources                api.Resources
	With, Secrets            []string
	Host                     bool
	Prepare                  bool          // run the session definition's prepare command and exit
	Idle                     time.Duration // stop the session after this long without requests (0: never)
}

func MCP(o MCPOptions) error {
	profile, on, res := o.Profile, o.On, o.Resources
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	spec := api.SessionSpec{Profile: profile, Resources: res, Client: clientLabel(), DNSSearch: dnsSearch(), NoStdin: o.Prepare}
	if spec.Secrets, spec.SecretFiles, err = resolveSecrets(cfg, o.With, o.Secrets, true, true); err != nil {
		return err
	}
	sc, err := LoadSessionConfig(profile)
	if err != nil {
		return err
	}
	if sc != nil {
		cmd := sc.Command
		if o.Prepare {
			if len(sc.Prepare) == 0 {
				return fmt.Errorf("session %s has no prepare command", profile)
			}
			cmd = sc.Prepare
		}
		data := map[string]string{}
		for name, ctr := range sc.Data {
			data[dataKey(name)] = ctr
		}
		spec.Def = &api.SessionDef{Command: cmd, Env: sc.Env, Data: data, Volumes: sc.Volumes,
			Devices: sc.Devices, NestedSandbox: sc.NestedSandbox, Network: sc.Network}
		if sc.Network == "client" {
			spec.Def.DNSSearch = dnsSearch()
		}
		if o.Environment == "" {
			o.Environment = sc.Environment
		}
		if spec.Resources.CPU == 0 {
			spec.Resources.CPU = sc.Resources.CPU
			if max := spec.Resources.CPUMax; max > 0 && spec.Resources.CPU > max {
				spec.Resources.CPU = max // the definition's default yields to an explicit --cpu-max
			}
		}
		if spec.Resources.CPUMax == 0 {
			spec.Resources.CPUMax = sc.Resources.CPUMax
		}
		if spec.Resources.Mem == 0 {
			spec.Resources.Mem, _ = units.ParseSize(sc.Resources.Mem)
		}
		res = spec.Resources
	} else if o.Prepare {
		return fmt.Errorf("--prepare needs a session definition in %s", filepath.Join(configDir(), "sessions", profile+".toml"))
	}
	if o.Host && sc != nil && (len(sc.Data) > 0 || len(sc.Volumes) > 0) {
		return fmt.Errorf("session %s mounts data sets or volumes, which only container sessions get; run it without --host", profile)
	}
	if o.Host { // runs on the runner itself: no environment to load at all
		spec.Runtime, spec.Environment = "host", nil
	} else if o.Environment != "" {
		if spec.Environment, err = LoadEnvDef("", o.Environment); err != nil {
			return err
		}
	}
	// A session prepared on a runner stays there (its volumes are filled there): pinned by
	// that runner's address, probed directly (not via discovery or names), waited for if busy.
	var pinned *Host
	if on == "" && sc != nil && len(sc.Volumes) > 0 && !o.Prepare {
		if url := preparedURL(profile); url != "" {
			pinned = pinHost(cfg, url)
			if found := probe(ctx, []*Host{pinned}); len(found) == 0 {
				return fmt.Errorf("session %s was prepared on %s, which isn't reachable now; wait for it, or run it elsewhere with --on NAME (and --prepare there)", profile, url)
			}
		}
	}
	launch := func() (*Host, api.Session, error) {
		var h *Host
		var se api.Session
		excluded := map[string]bool{}
		refused := map[string]bool{} // answered 403/422 (not paired, can never fit): skipped this launch
		synced := map[string]bool{}  // data sets already sent there: waiting for a free slot doesn't rescan them
		lastRefusal := ""
	pick:
		for {
			hosts := Discover(ctx, cfg)
			var avail []*Host
			for _, x := range hosts {
				if !excluded[x.URL] && !refused[x.URL] {
					avail = append(avail, x)
				}
			}
			hosts = avail
			if pinned != nil {
				h, err = pinned, nil
				if len(probe(ctx, []*Host{pinned})) == 0 { // fresh status for every attempt
					err = fmt.Errorf("the runner session %s was prepared on (%s) went away", profile, pinned.URL)
				}
			} else if on != "" {
				h, err = FindHost(hosts, on)
			} else if sc != nil {
				// Client-defined sessions run in a container unless --host: only runners that can
				// start one are candidates, warm data sets decide among those.
				want := api.JobSpec{Resources: res, Environment: spec.Environment, Runtime: "container", Network: sc.Network}
				if o.Host {
					want.Runtime = "host"
				}
				fit, _ := eligible(hosts, want)
				if len(fit) == 0 {
					fit = hosts // PickFor explains why none fits
				}
				// Warm data wins among runners that can take the session now; otherwise any that can.
				if h, err = PickFor(preferData(fit, sc.Data), want, ""); err != nil {
					h, err = PickFor(fit, want, "")
				}
			} else {
				h, err = Pick(hosts, res, profile, "", "session")
			}
			wantRT := "container"
			if o.Host {
				wantRT = "host"
			}
			busy := anyBusy(candidates(hosts, on, profile, api.JobSpec{Resources: res, Environment: spec.Environment, Runtime: wantRT}, sc != nil))
			if err != nil && len(excluded) == 0 && !busy && lastRefusal != "" {
				return nil, se, fmt.Errorf("%w (and %s)", err, lastRefusal)
			}
			if err != nil && (len(excluded) > 0 || busy) {
				// Every runner that can take it is full or under memory pressure right now: wait,
				// then a fresh round.
				fmt.Fprintf(os.Stderr, "offload: every runner that fits is full right now; waiting\n")
				select {
				case <-ctx.Done():
					return nil, se, ctx.Err()
				case <-time.After(5 * time.Second):
				}
				excluded = map[string]bool{}
				continue
			}
			if err == nil {
				err = checkProto(h)
			}
			if err != nil {
				return nil, se, err
			}
			if sc != nil && !synced[h.URL] {
				names := make([]string, 0, len(sc.Data))
				for name := range sc.Data {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					dc, ok := cfg.Data[name]
					if !ok {
						return nil, se, fmt.Errorf("session %s uses data set %q, which client.toml doesn't define ([data.%s])", profile, name, name)
					}
					if err := SyncData(ctx, h, name, dc, false); err != nil {
						var ae *APIError
						if errors.As(err, &ae) && ae.Code == 403 && on == "" && pinned == nil {
							fmt.Fprintf(os.Stderr, "offload: %s: %s; trying another runner\n", h.Name, ae.Msg)
							refused[h.URL] = true
							continue pick
						}
						return nil, se, err
					}
				}
				synced[h.URL] = true
			}
			for announced := false; ; {
				err := h.do(ctx, "POST", "/v1/sessions", spec, &se)
				var ae *APIError
				if errors.As(err, &ae) && ae.Code == 409 { // the environment is being built
					if !announced {
						fmt.Fprintf(os.Stderr, "offload: %s: %s\n", h.Name, ae.Msg)
						announced = true
					}
					select {
					case <-ctx.Done():
						return nil, se, ctx.Err()
					case <-time.After(3 * time.Second):
					}
					continue
				}
				if errors.As(err, &ae) && (ae.Code == 403 || ae.Code == 422) && on == "" && pinned == nil {
					// Not paired any more, or the session can never fit there: another runner.
					fmt.Fprintf(os.Stderr, "offload: %s: %s; trying another runner\n", h.Name, ae.Msg)
					refused[h.URL] = true
					lastRefusal = h.Name + ": " + ae.Msg
					continue pick
				}
				if errors.As(err, &ae) && ae.Code == 503 {
					// Full right now (capacity, session limit, upgrading): try the next runner, or
					// with --on wait for this one.
					fmt.Fprintf(os.Stderr, "offload: %s: %s\n", h.Name, ae.Msg)
					if on == "" && pinned == nil {
						excluded[h.URL] = true
					} else {
						select {
						case <-ctx.Done():
							return nil, se, ctx.Err()
						case <-time.After(5 * time.Second):
						}
					}
					continue pick
				}
				if err != nil {
					return nil, se, fmt.Errorf("%s: %w", h.Name, err)
				}
				break
			}
			break
		}
		fmt.Fprintf(os.Stderr, "offload: session %s:%s (%s)\n", h.Name, se.ID, profile)
		return h, se, nil
	}
	if o.Prepare {
		_ = os.Stdin.Close() // nothing to send; wait for the prepare command to finish
		h, se, err := launch()
		if err != nil {
			if ctx.Err() != nil {
				return interrupted(o)
			}
			return err
		}
		s := startSession(ctx, cfg, h, se, os.Stdout, true)
		defer s.stop()
		select {
		case <-ctx.Done():
			return interrupted(o)
		case err := <-s.ended():
			if err = s.finish(err); err == nil && sc != nil && len(sc.Volumes) > 0 {
				rememberPrepared(profile, h)
			}
			return err
		}
	}
	b := &bridge{idle: o.Idle, out: os.Stdout, open: func(stdout io.Writer) (mcpSession, error) {
		h, se, err := launch()
		if err != nil {
			return nil, err
		}
		return startSession(ctx, cfg, h, se, stdout, false), nil
	}}
	if err := b.run(ctx, os.Stdin); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// candidates are the runners that could ever take this launch (pinned name, profile of the
// session kind, capacity), whatever they are doing right now: only those are worth waiting for.
func candidates(hosts []*Host, on, profile string, want api.JobSpec, defined bool) []*Host {
	var out []*Host
	for _, h := range hosts {
		if on != "" && h.Name != on || h.Status == nil {
			continue
		}
		idle := *h
		st := *h.Status
		st.Accepting = true // judge what it can take, not what it is doing
		idle.Status = &st
		var err error
		if defined {
			_, err = PickFor([]*Host{&idle}, want, "")
		} else {
			_, err = Pick([]*Host{&idle}, want.Resources, profile, "", "session")
		}
		if err == nil {
			out = append(out, h)
		}
	}
	return out
}

// anyBusy reports whether a paired runner is temporarily refusing work (below its memory
// reserve, upgrading, reconciling): worth waiting for rather than giving up.
func anyBusy(hosts []*Host) bool {
	for _, h := range hosts {
		if h.Status != nil && !h.Status.Accepting && !(h.derived && h.Token == "") {
			return true
		}
	}
	return false
}

// interrupted is what Ctrl-C returns: fine for a bridged MCP server, a failure for --prepare
// (a script must not carry on as though provisioning finished).
func interrupted(o MCPOptions) error {
	if o.Prepare {
		return errors.New("prepare interrupted before it finished")
	}
	return nil
}

// prepareResult turns a finished prepare session's exit into the command's result: anything but
// a recorded exit 0 is a failure.
func prepareResult(ctx context.Context, h *Host, id string, prepare bool) error {
	what := "the MCP server"
	if prepare {
		what = "prepare"
	}
	var se api.Session
	if err := h.do(ctx, "GET", "/v1/sessions/"+id, nil, &se); err != nil {
		return fmt.Errorf("%s: can't read its result: %w", what, err)
	}
	if se.StopRequested { // stopped from elsewhere: whatever it exited with, it didn't finish
		return fmt.Errorf("%s was stopped (%s)", what, se.Reason)
	}
	if strings.HasPrefix(se.Reason, "out of memory") { // part of it was killed, whatever it exited
		return fmt.Errorf("%s ran out of memory (%s)", what, se.Reason)
	}
	if se.ExitCode == nil {
		return fmt.Errorf("%s ended without an exit status (%s)", what, se.Reason)
	}
	if *se.ExitCode != 0 {
		return &ExitError{Code: *se.ExitCode, What: what}
	}
	return nil
}

// ExitError is a remote process's nonzero exit status, passed on as offload's own.
type ExitError struct {
	Code int
	What string
}

func (e *ExitError) Error() string { return fmt.Sprintf("%s exited with status %d", e.What, e.Code) }

// A profile prepared on a runner (--prepare filled its persistent volumes there) keeps using
// that runner: another one would start from empty volumes. --on overrides it.
func preparedPath(profile string) string { return filepath.Join(stateDir(), "prepared", profile) }

func rememberPrepared(profile string, h *Host) {
	endpoint := h.URL
	if h.sock != "" {
		endpoint = "unix://" + h.sock // the local runner: its socket, not the synthetic URL
	}
	_ = os.MkdirAll(filepath.Dir(preparedPath(profile)), 0o700)
	_ = os.WriteFile(preparedPath(profile), []byte(endpoint+"\n"), 0o600)
}

// pinHost builds the host for a saved endpoint, with the token client.toml gives that endpoint
// (if any; the paired token otherwise).
func pinHost(cfg *Config, endpoint string) *Host {
	if endpoint == "http://local" { // saved by an older version for the local runner
		endpoint = "unix://" + localSocket()
	}
	token := ""
	for _, hc := range cfg.Hosts {
		if strings.TrimRight(hc.URL, "/") == endpoint {
			token = hc.Token
		}
	}
	return newHost("", endpoint, token, 0)
}

func preparedURL(profile string) string {
	b, err := os.ReadFile(preparedPath(profile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func preparedOn(profile string, hosts []*Host) *Host {
	url := preparedURL(profile)
	if url == "" {
		return nil
	}
	for _, h := range hosts {
		if h.URL == url || (h.sock != "" && url == "unix://"+h.sock) {
			return h
		}
	}
	return nil
}

// liveSession is one runner session: its stdout and stderr are followed until it ends.
type liveSession struct {
	h       *Host
	id      string
	seq     int64
	prepare bool
	ctx     context.Context
	cancel  context.CancelFunc
	out     chan error
	errOut  chan error
}

func startSession(ctx context.Context, cfg *Config, h *Host, se api.Session, stdout io.Writer, prepare bool) *liveSession {
	ctx, cancel := context.WithCancel(ctx)
	s := &liveSession{h: h, id: se.ID, prepare: prepare, ctx: ctx, cancel: cancel, out: make(chan error, 1), errOut: make(chan error, 1)}
	if se.Network == "client" { // the runner says so, whether the profile is ours or the runner's
		go serveCallbacks(ctx, h, "/v1/sessions/"+se.ID, cfg, true, map[string]bool{"dial": true})
	}
	go func() { s.out <- pumpOut(ctx, h, se.ID, "stdout", stdout) }()
	go func() { s.errOut <- pumpOut(ctx, h, se.ID, "stderr", os.Stderr) }()
	return s
}

func (s *liveSession) send(line []byte) error {
	s.seq++
	return sendStdin(s.ctx, s.h, s.id, s.seq, line)
}

func (s *liveSession) closeInput() error { return sendEOF(s.ctx, s.h, s.id, s.seq+1) }

func (s *liveSession) ended() <-chan error { return s.out }

func (s *liveSession) stop() {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.h.do(c, "DELETE", "/v1/sessions/"+s.id, nil, nil)
	s.cancel()
}

// finish is the session's result once its stdout ended with err. The rest of stderr is read
// first, within a bound (its last lines are often the diagnostic), and a failure there is
// reported, not swallowed. Reports are best-effort: if whoever reads our stderr stopped,
// writing must not hang us.
func (s *liveSession) finish(err error) error {
	warn := func(msg string) { go fmt.Fprintln(os.Stderr, msg) }
	select {
	case e := <-s.errOut:
		if e != nil {
			warn("offload: the session's stderr didn't arrive completely: " + e.Error())
		}
	case <-s.ctx.Done():
	case <-time.After(30 * time.Second):
		warn("offload: gave up waiting for the rest of the session's stderr")
	}
	if err != nil {
		return err
	}
	return prepareResult(s.ctx, s.h, s.id, s.prepare) // the server's own exit status
}

// maxMessage is the largest stdin line a session accepts (the runner's frame limit).
const maxMessage = 16 << 20

var errTooLong = errors.New("line too long")

// readLineMax reads one line (with its newline, if any), never buffering more than max bytes.
func readLineMax(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > max {
			return nil, errTooLong
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// remoteEnded is the session's input closing on the runner's side (the server closed its
// stdin, or the session is gone): not a local problem.
type remoteEnded struct{ msg string }

func (e *remoteEnded) Error() string { return "session ended: " + e.msg }

func sendEOF(ctx context.Context, h *Host, id string, seq int64) error {
	err := sendFrameReq(ctx, h, fmt.Sprintf("/v1/sessions/%s/stdin?seq=%d&eof=1", id, seq), nil)
	var ended *remoteEnded
	if errors.As(err, &ended) {
		return nil // its input was already closed, or it is gone: nothing left to close
	}
	return err
}

func sendStdin(ctx context.Context, h *Host, id string, seq int64, line []byte) error {
	return sendFrameReq(ctx, h, fmt.Sprintf("/v1/sessions/%s/stdin?seq=%d", id, seq), line)
}

func sendFrameReq(ctx context.Context, h *Host, url string, line []byte) error {
	for attempt := 0; ; attempt++ {
		err := h.do(ctx, "POST", url, bytes.NewReader(line), nil)
		if err == nil {
			return nil
		}
		var ae *APIError
		if errors.As(err, &ae) && (ae.Code == 403 || ae.Code == 404 || ae.Code == 410 || ae.Code == 412) {
			return &remoteEnded{ae.Msg}
		}
		if ctx.Err() != nil || attempt > 120 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// pumpOut copies a session stream, reconnecting at the last offset, until the session ends.
// recordingWriter remembers a write error, to tell it apart from a read error in io.Copy.
type recordingWriter struct {
	w   io.Writer
	err error
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	n, err := r.w.Write(p)
	if err != nil {
		r.err = err
	}
	return n, err
}

func pumpOut(ctx context.Context, h *Host, id, stream string, w io.Writer) error {
	var offset int64
	for {
		resp, err := h.request(ctx, "GET", fmt.Sprintf("/v1/sessions/%s/%s?follow=1&offset=%d", id, stream, offset), nil)
		if err == nil {
			ew := &recordingWriter{w: w}
			n, cerr := io.Copy(ew, resp.Body)
			resp.Body.Close()
			offset += n
			if ew.err != nil {
				return ew.err // our own output is gone (the MCP client went away)
			}
			err = cerr // a broken stream resumes at offset; only a clean end counts as drained
		}
		if err == nil {
			var se api.Session
			if gerr := h.do(ctx, "GET", "/v1/sessions/"+id, nil, &se); gerr == nil && api.Terminal(se.State) {
				// Read what was written between the stream's end and the session's (the last
				// reply), without following.
				if err := drainRest(ctx, h, func(off int64) string { return fmt.Sprintf("/v1/sessions/%s/%s?offset=%d", id, stream, off) }, offset, w); err != nil {
					return err
				}
				if se.Reason != "" && stream == "stdout" {
					fmt.Fprintf(os.Stderr, "offload: session ended: %s\n", se.Reason)
				}
				return nil
			}
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 404 {
			return fmt.Errorf("session %s is gone", id)
		}
		if errors.As(err, &ae) && (ae.Code == 412 || ae.Code == 403) { // upgraded, or pairing reset
			return fmt.Errorf("%s: %s (start the MCP server again)", h.Name, ae.Msg)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}
