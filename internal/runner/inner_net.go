package runner

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/RestartFU/offload/internal/netstack"
	"github.com/RestartFU/offload/internal/streamio"
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
	dial := func(proto, addr string) (io.ReadWriteCloser, error) {
		c, err := net.DialTimeout("unix", sock, 10*time.Second)
		if err != nil {
			return nil, err
		}
		path := fmt.Sprintf("/v1/%s/dial?proto=%s&addr=%s", owner, proto, url.QueryEscape(addr))
		return streamio.Upgrade(c, "runner", path, nil) // keeps CloseWrite for half-close
	}
	if err := netstack.Start(dial); err != nil {
		return err
	}
	netstack.WriteResolvConf()
	return nil
}
