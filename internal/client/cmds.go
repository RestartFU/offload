package client

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	goruntime "runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/RestartFU/offload/internal/api"
	"github.com/RestartFU/offload/internal/units"
)

func discover(ctx context.Context) ([]*Host, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	return Discover(ctx, cfg), nil
}

// Hosts prints every reachable runner and its load.
func Hosts() error {
	forgetHosts() // always rescan: this is how new runners show up
	hosts, err := discover(context.Background())
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		fmt.Println("no runners found")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNNER\tURL\tCPU (alloc/cap)\tMEM (alloc/cap)\tHOST AVAIL\tLOAD\tJOBS\tSESSIONS\tPROFILES\tSTATE")
	for _, h := range hosts {
		st := h.Status
		var profs []string
		for name := range st.Profiles {
			profs = append(profs, name)
		}
		state := "ok"
		if !st.Accepting {
			state = st.Reason
		}
		fmt.Fprintf(tw, "%s\t%s\t%.1f/%.0f\t%s/%s\t%s\t%.1f\t%d run, %d queued\t%d\t%s\t%s\n",
			h.Name, h.URL, st.Allocated.CPU, st.Capacity.CPU,
			units.FormatSize(st.Allocated.Mem), units.FormatSize(st.Capacity.Mem),
			units.FormatSize(st.MemAvailable), st.Load1, st.Running, st.Queued, st.Sessions,
			strings.Join(sortedStrings(profs), ","), state)
	}
	return tw.Flush()
}

func sortedStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

// Ps lists recent jobs and live sessions on every runner.
func Ps(all bool) error {
	ctx := context.Background()
	hosts, err := discover(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tAGE\tRES\tCOMMAND / PROFILE\tNOTE")
	q := ""
	if all {
		q = "?all=1"
	}
	for _, h := range hosts {
		var jobs []api.Job
		if err := h.do(ctx, "GET", "/v1/jobs"+q, nil, &jobs); err == nil {
			for _, j := range jobs {
				note := j.Reason
				if j.ExitCode != nil {
					note = strings.TrimSpace(fmt.Sprintf("exit %d %s", *j.ExitCode, note))
				}
				fmt.Fprintf(tw, "%s:%s\t%s\t%s\t%.0f/%.0f cpu %s\t%s\t%s\n", h.Name, j.ID, j.State,
					time.Since(j.CreatedAt).Round(time.Second), j.Resources.CPU, j.Resources.CPUMax,
					units.FormatSize(j.Resources.Mem), truncate(strings.Join(j.Spec.Argv, " "), 60), note)
			}
		}
		var sessions []api.Session
		if err := h.do(ctx, "GET", "/v1/sessions", nil, &sessions); err == nil {
			for _, s := range sessions {
				if api.Terminal(s.State) && !all {
					continue
				}
				fmt.Fprintf(tw, "%s:%s\t%s\t%s\t%.0f/%.0f cpu %s\tsession %s\t%d attached %s\n", h.Name, s.ID, s.State,
					time.Since(s.CreatedAt).Round(time.Second), s.Resources.CPU, s.Resources.CPUMax,
					units.FormatSize(s.Resources.Mem), s.Profile, s.Attached, s.Reason)
			}
		}
	}
	return tw.Flush()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Attach follows a job's output to the end and exits with its code. Ctrl-C detaches only.
func Attach(ref string) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hosts, err := discover(ctx)
	if err != nil {
		return 1, err
	}
	h, id, err := locateJob(ctx, hosts, ref)
	if err != nil {
		return 1, err
	}
	cfg, _ := LoadConfig()
	code, _, err := follow(ctx, h, id, false, false, cfg)
	return code, err
}

// Cancel stops a job.
func Cancel(ref string) error {
	ctx := context.Background()
	hosts, err := discover(ctx)
	if err != nil {
		return err
	}
	h, id, err := locateJob(ctx, hosts, ref)
	if err != nil {
		return err
	}
	if strings.HasPrefix(id, "s-") {
		return h.do(ctx, "DELETE", "/v1/sessions/"+id, nil, nil)
	}
	return h.do(ctx, "DELETE", "/v1/jobs/"+id, nil, nil)
}

// Get downloads a finished job's artifacts.
func Get(ref, dest string) error {
	ctx := context.Background()
	hosts, err := discover(ctx)
	if err != nil {
		return err
	}
	h, id, err := locateJob(ctx, hosts, ref)
	if err != nil {
		return err
	}
	return fetchArtifacts(ctx, h, id, dest)
}

// Upgrade pushes a binary (this one by default) to runners, which swap it in and restart; running
// jobs continue. A runner of another OS/arch needs a matching build passed with --binary.
func Upgrade(names []string, all bool, binary string) error {
	ctx := context.Background()
	hosts, err := discover(ctx)
	if err != nil {
		return err
	}
	self := binary
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return err
		}
	}
	var targets []*Host
	if all {
		targets = hosts
	}
	for _, n := range names {
		h, err := FindHost(hosts, n)
		if err != nil {
			return err
		}
		targets = append(targets, h)
	}
	if len(targets) == 0 {
		return fmt.Errorf("name a runner or pass --all")
	}
	for _, h := range targets {
		if binary == "" && h.Status.OS != "" && (h.Status.OS != goruntime.GOOS || h.Status.Arch != goruntime.GOARCH) {
			return fmt.Errorf("%s is %s/%s; build a matching binary (CGO_ENABLED=0 GOOS=%s GOARCH=%s go build) and pass --binary",
				h.Name, h.Status.OS, h.Status.Arch, h.Status.OS, h.Status.Arch)
		}
		f, err := os.Open(self)
		if err != nil {
			return err
		}
		err = h.do(ctx, "PUT", "/v1/admin/binary", f, nil)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
		fmt.Printf("%s: upgraded (was %s)\n", h.Name, h.Status.Version)
	}
	return nil
}
