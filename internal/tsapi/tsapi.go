// Package tsapi is a minimal client for the local tailscaled API: who is calling, and which
// peers exist. It talks to the tailscaled socket and falls back to the tailscale CLI.
package tsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"
)

var sockets = []string{"/var/run/tailscale/tailscaled.sock", "/run/tailscale/tailscaled.sock"}

// WhoIs identifies the tailnet node and user behind a remote address.
type WhoIs struct {
	Node struct {
		Name         string   `json:"Name"`
		ComputedName string   `json:"ComputedName"`
		Tags         []string `json:"Tags"`
	} `json:"Node"`
	UserProfile struct {
		LoginName string `json:"LoginName"`
	} `json:"UserProfile"`
}

// Peer is one node in the tailnet status.
type Peer struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
	UserID       int64    `json:"UserID"`
	Tags         []string `json:"Tags"`
}

// Status is the subset of tailscaled's status we use.
type Status struct {
	BackendState string           `json:"BackendState"`
	Self         *Peer            `json:"Self"`
	Peer         map[string]*Peer `json:"Peer"`
	User         map[string]struct {
		LoginName string `json:"LoginName"`
	} `json:"User"`
}

func socketPath() string {
	for _, s := range sockets {
		if _, err := os.Stat(s); err == nil {
			return s
		}
	}
	return ""
}

func get(ctx context.Context, path string, out any) error {
	sock := socketPath()
	if sock == "" {
		return fmt.Errorf("tailscaled socket not found")
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://local-tailscaled.sock"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("tailscaled %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func cli(ctx context.Context, out any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "tailscale", args...).Output()
	if err != nil {
		return fmt.Errorf("tailscale %v: %w", args, err)
	}
	return json.Unmarshal(b, out)
}

// LookupWhoIs identifies the caller at addr ("ip:port").
func LookupWhoIs(ctx context.Context, addr string) (*WhoIs, error) {
	var w WhoIs
	if err := get(ctx, "/localapi/v0/whois?addr="+url.QueryEscape(addr), &w); err == nil {
		return &w, nil
	}
	if err := cli(ctx, &w, "whois", "--json", addr); err != nil {
		return nil, err
	}
	return &w, nil
}

// GetStatus returns this node's tailnet status, including peers.
func GetStatus(ctx context.Context) (*Status, error) {
	var s Status
	if err := get(ctx, "/localapi/v0/status", &s); err == nil {
		return &s, nil
	}
	if err := cli(ctx, &s, "status", "--json"); err != nil {
		return nil, err
	}
	return &s, nil
}

// SelfLogin returns the login name owning this node (e.g. "someone@github").
func (s *Status) SelfLogin() string {
	if s.Self == nil {
		return ""
	}
	if u, ok := s.User[fmt.Sprint(s.Self.UserID)]; ok {
		return u.LoginName
	}
	return ""
}
