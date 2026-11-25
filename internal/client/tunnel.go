package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/streamio"
)

// Client side of network = "client": the runner asks us to dial a destination for the job; we
// dial it from this machine and connect the result back to the runner as a raw stream.

// clientIP is the job-side address that stands for this machine (see internal/netstack).
const clientIP = "10.233.0.1"

// mapAddr turns a job-side destination into one dialable here: the client address means this
// machine's localhost, and its port 53 means this machine's resolver.
func mapAddr(proto, addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != clientIP {
		return addr
	}
	if port == "53" {
		return net.JoinHostPort(localResolver(), "53")
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// dnsSearch is this machine's DNS search list, so short names (tailnet hosts) resolve in the job.
func dnsSearch() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && (f[0] == "search" || f[0] == "domain") {
			out = append(out, f[1:]...)
		}
	}
	return out
}

var tailnetRanges = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e0::/48")}

// blockedTarget refuses connections a job must never make with this machine's identity: any
// runner's API (the tailnet runner port, or a configured runner URL), which would otherwise accept
// the job as the user who owns this laptop. Everything else goes through.
func blockedTarget(target string, cfg *Config) string {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "bad address"
	}
	runnerPort := fmt.Sprint(api.DefaultPort)
	if cfg != nil && cfg.Port != 0 {
		runnerPort = fmt.Sprint(cfg.Port)
	}
	if ip, err := netip.ParseAddr(host); err == nil && port == runnerPort {
		ip = ip.Unmap()
		for _, p := range tailnetRanges {
			if p.Contains(ip) {
				return "refused: jobs can't reach offload runners through this machine"
			}
		}
	}
	if cfg != nil && len(cfg.Hosts) > 0 {
		norm := target
		if ip, err := netip.ParseAddr(host); err == nil {
			norm = net.JoinHostPort(ip.Unmap().String(), port)
		}
		if configuredRunnerAddrs(cfg)[norm] {
			return "refused: jobs can't reach offload runners through this machine"
		}
	}
	return ""
}

// resolveTarget resolves a destination here and checks every address it has, so a name can't
// smuggle a dial to a runner past blockedTarget. It returns the checked address to dial.
func resolveTarget(ctx context.Context, target string, cfg *Config) (string, string) {
	if why := blockedTarget(target, cfg); why != "" {
		return "", why
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", "bad address"
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return target, ""
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Sprintf("can't resolve %s: %v", host, err)
	}
	for _, ip := range ips {
		if why := blockedTarget(net.JoinHostPort(ip.Unmap().String(), port), cfg); why != "" {
			return "", why
		}
	}
	return net.JoinHostPort(ips[0].Unmap().String(), port), ""
}

// runnerAddrs are the addresses of the configured runners (resolved, cached for a minute).
var runnerAddrs = struct {
	sync.Mutex
	at  time.Time
	set map[string]bool
}{}

func configuredRunnerAddrs(cfg *Config) map[string]bool {
	runnerAddrs.Lock()
	defer runnerAddrs.Unlock()
	if runnerAddrs.set != nil && time.Since(runnerAddrs.at) < time.Minute {
		return runnerAddrs.set
	}
	set := map[string]bool{}
	for _, hc := range cfg.Hosts {
		u, err := url.Parse(hc.URL)
		if err != nil || u.Host == "" {
			continue
		}
		host, port := u.Hostname(), u.Port()
		if port == "" {
			port = map[string]string{"https": "443"}[u.Scheme]
			if port == "" {
				port = "80"
			}
		}
		set[net.JoinHostPort(host, port)] = true
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ips, _ := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		cancel()
		for _, ip := range ips {
			set[net.JoinHostPort(ip.Unmap().String(), port)] = true
		}
	}
	runnerAddrs.at, runnerAddrs.set = time.Now(), set
	return set
}

// localResolver is the first nameserver in /etc/resolv.conf (e.g. systemd-resolved, which also
// answers tailnet names).
func localResolver() string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == "nameserver" && net.ParseIP(f[1]) != nil {
				return f[1]
			}
		}
	}
	return "127.0.0.53"
}

// serveDial handles one dial request and returns the callback answer ("ok" or the error).
func serveDial(ctx context.Context, h *Host, data []byte, cfg *Config) string {
	var req struct {
		Stream string `json:"stream"`
		Proto  string `json:"proto"`
		Addr   string `json:"addr"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return "bad request"
	}
	target, why := resolveTarget(ctx, mapAddr(req.Proto, req.Addr), cfg)
	if why != "" {
		return why
	}
	var conn net.Conn
	var err error
	switch req.Proto {
	case "tcp":
		conn, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", target)
	case "udp":
		conn, err = (&net.Dialer{}).DialContext(ctx, "udp", target)
	default:
		return "unsupported protocol"
	}
	if err != nil {
		return err.Error()
	}
	stream, err := openStream(ctx, h, req.Stream)
	if err != nil {
		conn.Close()
		return err.Error()
	}
	// When the workload ends (or this client stops serving it), its connections end too, even a
	// half-closed one still waiting for the far side.
	stopOnEnd := context.AfterFunc(ctx, func() { stream.Close(); conn.Close() })
	go func() {
		defer stopOnEnd()
		if req.Proto == "udp" {
			streamio.RelayDatagrams(conn.(*net.UDPConn), stream, 2*time.Minute)
		} else {
			streamio.Splice(stream, conn)
		}
	}()
	return "ok"
}

// openStream connects our leg of a dial to the runner: an HTTP upgrade on a raw connection, so
// half-closes can travel through it.
func openStream(ctx context.Context, h *Host, id string) (net.Conn, error) {
	c, err := h.dialRaw(ctx)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	if h.Token != "" {
		hdr.Set("Authorization", "Bearer "+h.Token)
	}
	hdr.Set(api.ProtoHeader, fmt.Sprint(api.Proto))
	return streamio.Upgrade(c, h.hostHeader(), "/v1/streams/"+id, hdr)
}
