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
	"sync"
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
	Prepare                  bool // run the session definition's prepare command and exit
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
	if o.Environment != "" {
		if spec.Environment, err = LoadEnvDef("", o.Environment); err != nil {
			return err
		}
	}
	if o.Host {
		spec.Runtime, spec.Environment = "host", nil
	}
	var h *Host
	var se api.Session
	excluded := map[string]bool{}
	refused := map[string]bool{} // answered 403/422 (not paired, can never fit): skipped this launch
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
		if on != "" {
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
			return fmt.Errorf("%w (and %s)", err, lastRefusal)
		}
		if err != nil && (len(excluded) > 0 || busy) {
			// Every runner that can take it is full or under memory pressure right now: wait,
			// then a fresh round.
			fmt.Fprintf(os.Stderr, "offload: every runner that fits is full right now; waiting\n")
			select {
			case <-ctx.Done():
				return interrupted(o)
			case <-time.After(5 * time.Second):
			}
			excluded = map[string]bool{}
			continue
		}
		if err == nil {
			err = checkProto(h)
		}
		if err != nil {
			return err
		}
		if sc != nil {
			names := make([]string, 0, len(sc.Data))
			for name := range sc.Data {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				dc, ok := cfg.Data[name]
				if !ok {
					return fmt.Errorf("session %s uses data set %q, which client.toml doesn't define ([data.%s])", profile, name, name)
				}
				if err := SyncData(ctx, h, name, dc, false); err != nil {
					var ae *APIError
					if errors.As(err, &ae) && ae.Code == 403 && on == "" {
						fmt.Fprintf(os.Stderr, "offload: %s: %s; trying another runner\n", h.Name, ae.Msg)
						refused[h.URL] = true
						continue pick
					}
					return err
				}
			}
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
					return interrupted(o)
				case <-time.After(3 * time.Second):
				}
				continue
			}
			if errors.As(err, &ae) && (ae.Code == 403 || ae.Code == 422) && on == "" {
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
				if on == "" {
					excluded[h.URL] = true
				} else {
					select {
					case <-ctx.Done():
						return interrupted(o)
					case <-time.After(5 * time.Second):
					}
				}
				continue pick
			}
			if err != nil {
				return fmt.Errorf("%s: %w", h.Name, err)
			}
			break
		}
		break
	}
	fmt.Fprintf(os.Stderr, "offload: session %s:%s (%s)\n", h.Name, se.ID, profile)
	if se.Network == "client" { // the runner says so, whether the profile is ours or the runner's
		go serveCallbacks(ctx, h, "/v1/sessions/"+se.ID, cfg, true, map[string]bool{"dial": true})
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = h.do(c, "DELETE", "/v1/sessions/"+se.ID, nil, nil)
	}()

	out := &activityWriter{w: os.Stdout, started: time.Now()}
	done := make(chan error, 2)
	inDone := make(chan error, 1)
	errDone := make(chan struct{})
	go func() { done <- pumpOut(ctx, h, se.ID, "stdout", out) }()
	go func() { _ = pumpOut(ctx, h, se.ID, "stderr", os.Stderr); close(errDone) }()
	// When the session ends, let stderr catch up (its last lines are often the diagnostic),
	// but not forever.
	drainStderr := func() {
		select {
		case <-errDone:
		case <-time.After(5 * time.Second):
		}
	}
	if o.Prepare {
		_ = os.Stdin.Close() // nothing to send; wait for the prepare command to finish
	} else {
		go func() { inDone <- pumpIn(ctx, h, se.ID, os.Stdin) }()
	}
	select {
	case <-ctx.Done():
		return interrupted(o)
	case err := <-done:
		drainStderr()
		if err == nil && o.Prepare {
			return prepareResult(ctx, h, se.ID)
		}
		return err
	case err := <-inDone:
		if err != nil {
			drainStderr() // the server's last words (why it stopped reading) are usually there
			return err
		}
		// Input closed: let answers already in flight arrive, then end the session. Wait at
		// least 3s from the start (a fresh container may still be starting), stop once output
		// has been quiet for a second, and never wait more than 10s.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if time.Since(out.started) >= 3*time.Second && time.Since(out.last()) >= time.Second {
				break
			}
			select {
			case <-ctx.Done():
				return nil
			case err := <-done:
				drainStderr()
				return err
			case <-time.After(100 * time.Millisecond):
			}
		}
		return nil
	}
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
func prepareResult(ctx context.Context, h *Host, id string) error {
	var se api.Session
	if err := h.do(ctx, "GET", "/v1/sessions/"+id, nil, &se); err != nil {
		return fmt.Errorf("prepare: can't read its result: %w", err)
	}
	if se.ExitCode == nil {
		return fmt.Errorf("prepare ended without an exit status (%s)", se.Reason)
	}
	if *se.ExitCode != 0 {
		return fmt.Errorf("prepare failed: exit %d", *se.ExitCode)
	}
	return nil
}

// activityWriter remembers when output last arrived.
type activityWriter struct {
	w       io.Writer
	mu      sync.Mutex
	at      time.Time
	started time.Time
}

func (a *activityWriter) Write(p []byte) (int, error) {
	a.mu.Lock()
	a.at = time.Now()
	a.mu.Unlock()
	return a.w.Write(p)
}

func (a *activityWriter) last() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.at
}

func pumpIn(ctx context.Context, h *Host, id string, in io.Reader) error {
	r := bufio.NewReaderSize(in, 1<<20)
	var seq int64
	for {
		line, err := readLineMax(r, maxMessage)
		if errors.Is(err, errTooLong) {
			return fmt.Errorf("an MCP message is over the %d MiB a session accepts", maxMessage>>20)
		}
		if len(line) > 0 {
			seq++
			if serr := sendStdin(ctx, h, id, seq, line); serr != nil {
				return serr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil // the MCP client closed stdin: end the session
			}
			return err
		}
	}
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

func sendStdin(ctx context.Context, h *Host, id string, seq int64, line []byte) error {
	for attempt := 0; ; attempt++ {
		err := h.do(ctx, "POST", fmt.Sprintf("/v1/sessions/%s/stdin?seq=%d", id, seq), bytes.NewReader(line), nil)
		if err == nil {
			return nil
		}
		var ae *APIError
		if errors.As(err, &ae) && (ae.Code == 403 || ae.Code == 404 || ae.Code == 410 || ae.Code == 412) {
			return fmt.Errorf("session ended: %s", ae.Msg)
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
				if resp, rerr := h.request(ctx, "GET", fmt.Sprintf("/v1/sessions/%s/%s?offset=%d", id, stream, offset), nil); rerr == nil {
					_, _ = io.Copy(w, resp.Body)
					resp.Body.Close()
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
