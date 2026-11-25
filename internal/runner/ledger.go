package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// The ledger keeps one line per finished job or session (ledger.jsonl in the state directory)
// for storage.ledger.retain, long after the job records themselves are collected. It feeds
// statistics: what ran, for how long, how long it waited, and what it used.

const ledgerMaxRead = 50000 // entries one request returns at most (the newest)

func (s *Server) ledgerPath() string { return filepath.Join(s.cfg.StateDir, "ledger.jsonl") }

// recordJobLocked appends a finished job to the ledger.
func (s *Server) recordJobLocked(j *job) {
	e := api.LedgerEntry{
		ID: j.ID, Kind: "job", Command: commandSig(j.Spec.Argv), Workspace: j.Spec.WorkspaceKey,
		Profile: j.Spec.Profile, Client: j.Spec.Client, Caller: j.Caller, Runtime: j.Runtime,
		State: j.State, ExitCode: j.ExitCode, Reason: j.Reason, OOM: isOOMReason(j.Reason),
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, Resources: j.Resources, MemFrom: j.MemFrom,
		PeakMem: j.PeakMem, CPUSeconds: j.CPUSeconds, SyncFiles: j.SyncFiles, Uploaded: j.Uploaded,
	}
	s.appendLedger(e, j.FinishedAt)
}

// recordSessionLocked appends a finished session to the ledger.
func (s *Server) recordSessionLocked(se *session) {
	started := se.CreatedAt
	e := api.LedgerEntry{
		ID: se.ID, Kind: "session", Command: se.Profile, Profile: se.Profile, Client: se.Client,
		Caller: se.Caller, Runtime: se.Runtime, State: se.State, ExitCode: se.ExitCode, Reason: se.Reason,
		OOM: isOOMReason(se.Reason), CreatedAt: se.CreatedAt, StartedAt: &started, Resources: se.Resources,
		CPUSeconds: s.takeCPUSeconds(se.ID),
	}
	s.appendLedger(e, se.FinishedAt)
}

func isOOMReason(r string) bool { return strings.HasPrefix(r, "out of memory") }

func (s *Server) appendLedger(e api.LedgerEntry, finished *time.Time) {
	e.FinishedAt = time.Now()
	if finished != nil {
		e.FinishedAt = *finished
	}
	if e.StartedAt != nil {
		e.QueueWait = max(0, e.StartedAt.Sub(e.CreatedAt).Seconds())
		e.Duration = max(0, e.FinishedAt.Sub(*e.StartedAt).Seconds())
	} else {
		e.QueueWait = max(0, e.FinishedAt.Sub(e.CreatedAt).Seconds())
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.ledgerPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		log.Printf("ledger: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Printf("ledger: %v", err)
	}
}

// readLedger returns the entries that finished at or after since, oldest first. A line being
// appended right now (or one cut short by a crash) is skipped.
func (s *Server) readLedger(since time.Time) []api.LedgerEntry {
	out := []api.LedgerEntry{}
	f, err := os.Open(s.ledgerPath())
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e api.LedgerEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.FinishedAt.Before(since) {
			continue
		}
		out = append(out, e)
	}
	if len(out) > ledgerMaxRead {
		out = out[len(out)-ledgerMaxRead:]
	}
	return out
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-24 * time.Hour)
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, 400, "bad since: want RFC 3339, e.g. 2026-01-02T15:04:05Z")
			return
		}
		since = t
	}
	writeJSON(w, s.readLedger(since))
}

// gcLedgerLocked drops entries older than the retention. The file is only rewritten when its
// first entry has expired. Appends hold s.mu too, so none is lost to the rewrite.
func (s *Server) gcLedgerLocked() {
	cutoff := time.Now().Add(-s.cfg.ledgerRetain)
	f, err := os.Open(s.ledgerPath())
	if err != nil {
		return
	}
	first, _ := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	f.Close()
	var e api.LedgerEntry
	if json.Unmarshal(first, &e) == nil && !e.FinishedAt.Before(cutoff) {
		return // the oldest entry is still wanted: nothing to drop
	}
	b, err := os.ReadFile(s.ledgerPath())
	if err != nil {
		return
	}
	var keep bytes.Buffer
	for _, line := range bytes.Split(b, []byte("\n")) {
		if json.Unmarshal(line, &e) == nil && !e.FinishedAt.Before(cutoff) {
			keep.Write(line)
			keep.WriteByte('\n')
		}
	}
	tmp := s.ledgerPath() + ".tmp"
	if err := os.WriteFile(tmp, keep.Bytes(), 0o600); err == nil {
		err = os.Rename(tmp, s.ledgerPath())
		if err != nil {
			log.Printf("ledger gc: %v", err)
		}
	}
}
