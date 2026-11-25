package client

import (
	"testing"

	"github.com/RestartFU/offload/internal/api"
)

func testHost(name string, cpu float64, alloc, pending api.Resources, warm ...string) *Host {
	h := newHost(name, "http://"+name+":7420", "tok", 0)
	h.Status = &api.Status{
		Name: name, Proto: api.Proto, Accepting: true, OS: "linux", Arch: "amd64",
		Capacity: api.Resources{CPU: cpu, Mem: 32 << 30}, Defaults: api.Resources{CPU: 2, Mem: 1 << 30},
		Allocated: alloc, Pending: pending, MemAvailable: 40 << 30, MemReserve: 2 << 30,
		WarmKeys: warm, Profiles: map[string]api.ProfileInfo{},
	}
	return h
}

// A job still uploading or queued has claimed its CPUs: a second submit a few seconds later must
// go to the idle runner, even past a warm workspace.
func TestPickCountsPendingJobs(t *testing.T) {
	want := api.Resources{CPU: 16, Mem: 4 << 30}
	busy := testHost("vps", 30, api.Resources{}, want, "repo")
	idle := testHost("zeno", 22, api.Resources{}, api.Resources{})
	h, err := pick([]*Host{busy, idle}, want, "", "repo", "job", nil)
	if err != nil || h.Name != "zeno" {
		t.Fatalf("picked %v, %v; want zeno", h, err)
	}
	// Without the pending job the warm runner wins, as before.
	busy.Status.Pending = api.Resources{}
	if h, _ := pick([]*Host{busy, idle}, want, "", "repo", "job", nil); h.Name != "vps" {
		t.Fatalf("picked %s; want the warm vps", h.Name)
	}
}

// An overbooking runner with idle cores can start a job past its guaranteed capacity.
func TestPickOverbookedIdleRunner(t *testing.T) {
	vps := testHost("vps", 30, api.Resources{CPU: 16}, api.Resources{})
	vps.Status.CPUOvercommit, vps.Status.CPUIdle = 2, 25
	if f := vps.free(); f.CPU < 16 {
		t.Fatalf("free %.1f; want ≥16 with 25 idle cores", f.CPU)
	}
	vps.Status.CPUIdle = 5
	if f := vps.free(); f.CPU >= 16 {
		t.Fatalf("free %.1f with only 5 idle cores", f.CPU)
	}
}

// A runner with unbooked room beats one that could only take the job by overbooking.
func TestPickPrefersUnbookedRoom(t *testing.T) {
	want := api.Resources{CPU: 8, Mem: 1 << 30}
	vps := testHost("vps", 30, api.Resources{CPU: 24}, api.Resources{})
	vps.Status.CPUOvercommit, vps.Status.CPUIdle = 2, 20
	zeno := testHost("zeno", 22, api.Resources{CPU: 12}, api.Resources{})
	if h, _ := pick([]*Host{vps, zeno}, want, "", "", "job", nil); h.Name != "zeno" {
		t.Fatalf("picked %s; want zeno, which has 10 unbooked cores", h.Name)
	}
}
