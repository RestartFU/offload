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

// Warm on both (vps with more image variants), vps busy, zeno idle: the job goes where its cores
// are. Slots per variant used to stack the warm bonus and pin work to the busy runner.
func TestPickSpreadsToIdleRunner(t *testing.T) {
	want := api.Resources{CPU: 4, Mem: 1 << 30}
	vps := testHost("vps", 30, api.Resources{CPU: 20}, api.Resources{}, "repo", "repo--a", "repo--b", "repo--c", "repo--d")
	vps.Status.CPUOvercommit, vps.Status.CPUIdle = 2, 6
	zeno := testHost("zeno", 22, api.Resources{}, api.Resources{}, "repo--a")
	zeno.Status.CPUOvercommit, zeno.Status.CPUIdle = 2, 22
	if h, _ := pick([]*Host{vps, zeno}, want, "", "repo", "job", nil); h.Name != "zeno" {
		t.Fatalf("picked %s; want the idle zeno", h.Name)
	}
	// Equally idle: the warm runner still wins over a cold one.
	vps.Status.Allocated, vps.Status.CPUIdle = api.Resources{}, 22
	zeno.Status.WarmKeys = nil
	if h, _ := pick([]*Host{vps, zeno}, want, "", "repo", "job", nil); h.Name != "vps" {
		t.Fatalf("picked %s; want the warm vps", h.Name)
	}
}

// A runner reporting its memory room is taken at its word: its limits may add up past capacity
// because it books jobs at their usual peak.
func TestFreeUsesMemRoom(t *testing.T) {
	vps := testHost("vps", 30, api.Resources{Mem: 32 << 30}, api.Resources{Mem: 2 << 30})
	if f := vps.free(); f.Mem > 0 {
		t.Fatalf("older runner, fully allocated: free %d", f.Mem)
	}
	room := int64(12 << 30)
	vps.Status.MemRoom = &room
	if f := vps.free(); f.Mem != 10<<30 {
		t.Fatalf("free %d; want room less pending, 10G", f.Mem)
	}
}
