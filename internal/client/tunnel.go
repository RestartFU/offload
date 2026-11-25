package client

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

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
func serveDial(ctx context.Context, h *Host, data []byte) string {
	var req struct {
		Stream string `json:"stream"`
		Proto  string `json:"proto"`
		Addr   string `json:"addr"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return "bad request"
	}
	target := mapAddr(req.Proto, req.Addr)
	var conn net.Conn
	var err error
	switch req.Proto {
	case "tcp":
		conn, err = net.DialTimeout("tcp", target, 10*time.Second)
	case "udp":
		conn, err = net.Dial("udp", target)
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
	return streamio.Upgrade(c, h.hostHeader(), "/v1/streams/"+id, hdr)
}
