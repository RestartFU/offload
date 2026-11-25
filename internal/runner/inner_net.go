package runner

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/RestartFU/offload/internal/netstack"
)

// startClientNet brings up the client-backed network inside a container (see netstack): every
// connection becomes a dial request to the runner, which has the attached client open it.
func startClientNet() error {
	sock := os.Getenv("OFFLOAD_RUNNER_SOCK")
	owner := "jobs/" + os.Getenv("OFFLOAD_JOB")
	if id := os.Getenv("OFFLOAD_SESSION"); id != "" {
		owner = "sessions/" + id
	}
	if sock == "" || strings.HasSuffix(owner, "/") {
		return fmt.Errorf("no runner socket or job id")
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	dial := func(proto, addr string) (io.ReadWriteCloser, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		u := fmt.Sprintf("http://runner/v1/%s/dial?proto=%s&addr=%s", owner, proto, url.QueryEscape(addr))
		req, err := http.NewRequestWithContext(ctx, "POST", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "offload-stream")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusSwitchingProtocols {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("dial %s %s: %s", proto, addr, strings.TrimSpace(string(b)))
		}
		rwc, ok := resp.Body.(io.ReadWriteCloser)
		if !ok {
			resp.Body.Close()
			return nil, fmt.Errorf("dial %s: no stream", addr)
		}
		return rwc, nil
	}
	if err := netstack.Start(dial); err != nil {
		return err
	}
	netstack.WriteResolvConf()
	return nil
}
