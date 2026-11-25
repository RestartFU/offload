package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/RestartFU/offload/internal/api"
)

// The ledger keeps one line per finished job or session (ledger.jsonl in the state directory)
// for storage.ledger.retain, long after the job records themselves are collected. It feeds
// statistics: what ran, for how long, how long it waited, and what it used.
//
// Appends hold ledgerMu and write a whole line in one call. A crash can still leave a torn
// last line; the next append starts a fresh line first, and readers skip what doesn't parse.
// A workload can be recorded twice: a crash between the append and saving its terminal state
// finishes it again after the restart, with the restart's time. Readers keep the first entry.

const (
	ledgerMaxRead  = 50000   // entries one request returns at most (the newest)
	ledgerMaxLine  = 1 << 20 // longer lines are skipped
	ledgerMaxField = 512     // free-form text fields are cut to this many bytes
	ledgerGCSlack  = time.Hour
)

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
		PeakMem: se.peakMem, CPUSeconds: se.cpuSecs,
	}
	if cpu := s.takeCPUSeconds(se.ID); e.CPUSeconds == 0 {
		e.CPUSeconds = cpu // from the last sample: an older wrapper, or no exit record
	}
	s.appendLedger(e, se.FinishedAt)
}

func isOOMReason(r string) bool { return strings.HasPrefix(r, "out of memory") }

// clip cuts s to at most n bytes, on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

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
	for _, f := range []*string{&e.Command, &e.Workspace, &e.Profile, &e.Client, &e.Caller, &e.Reason} {
		*f = clip(*f, ledgerMaxField)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	f, err := os.OpenFile(s.ledgerPath(), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		log.Printf("ledger: %v", err)
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err == nil && last[0] != '\n' {
			b = append([]byte{'\n'}, b...) // a torn line from a crash: don't glue this entry to it
		}
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Printf("ledger: %v", err)
	}
}

// readLine returns the next line without its newline. A line longer than ledgerMaxLine comes
// back as nil, skipped. At the end it returns io.EOF, with any unterminated last line.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !long {
			if len(line)+len(chunk) > ledgerMaxLine+1 {
				long, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSuffix(line, []byte("\n")), err
	}
}

// readLedger returns the entries that finished at or after since, oldest first, at most the
// newest ledgerMaxRead of them (truncated says so), and the first entry per workload.
func (s *Server) readLedger(since time.Time) (out []api.LedgerEntry, truncated bool, err error) {
	out = []api.LedgerEntry{}
	f, err := os.Open(s.ledgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return out, false, nil
	} else if err != nil {
		return nil, false, err
	}
	defer f.Close()
	at := map[string]int{} // id -> index in out
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, rerr := readLine(br)
		var e api.LedgerEntry
		if len(line) > 0 && json.Unmarshal(line, &e) == nil && !e.FinishedAt.Before(since) {
			if _, ok := at[e.ID]; !ok {
				at[e.ID] = len(out)
				out = append(out, e)
			}
			if len(out) >= 2*ledgerMaxRead { // keep memory bounded: drop the oldest half
				out = append(out[:0:0], out[len(out)-ledgerMaxRead:]...)
				truncated = true
				clear(at)
				for i, e := range out {
					at[e.ID] = i
				}
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		} else if rerr != nil {
			return nil, false, rerr
		}
	}
	if len(out) > ledgerMaxRead {
		out, truncated = out[len(out)-ledgerMaxRead:], true
	}
	return out, truncated, nil
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
	rows, truncated, err := s.readLedger(since)
	if err != nil {
		writeErr(w, 500, "ledger: "+err.Error())
		return
	}
	if truncated {
		w.Header().Set("X-Offload-Truncated", "1") // only the newest ledgerMaxRead entries
	}
	writeJSON(w, rows)
}

// gcLedger drops entries older than the retention. It rewrites the file only once its oldest
// entry is ledgerGCSlack past expiry, and without holding ledgerMu or s.mu while it filters:
// it copies what was there when it started, then (under ledgerMu, briefly) the lines appended
// meanwhile, and swaps the file in. Only the GC loop calls it.
func (s *Server) gcLedger() {
	path := s.ledgerPath()
	cutoff := time.Now().Add(-s.cfg.ledgerRetain)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	first, _ := readLine(bufio.NewReaderSize(f, 64<<10))
	var e api.LedgerEntry
	if json.Unmarshal(first, &e) == nil && !e.FinishedAt.Before(cutoff.Add(-ledgerGCSlack)) {
		return // the oldest entry is still wanted, or only just expired
	}
	s.ledgerMu.Lock() // no append is half-written at this size
	st, err := f.Stat()
	s.ledgerMu.Unlock()
	if err != nil {
		return
	}
	size := st.Size()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	tmpPath := path + ".tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("ledger gc: %v", err)
		return
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()
	w := bufio.NewWriterSize(tmp, 64<<10)
	br := bufio.NewReaderSize(io.LimitReader(f, size), 64<<10)
	for {
		line, rerr := readLine(br)
		if len(line) > 0 && json.Unmarshal(line, &e) == nil && !e.FinishedAt.Before(cutoff) {
			w.Write(line)
			w.WriteByte('\n')
		}
		if rerr != nil {
			break
		}
	}
	if err := w.Flush(); err != nil {
		log.Printf("ledger gc: %v", err)
		return
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	if _, err := f.Seek(size, io.SeekStart); err != nil {
		return
	}
	if _, err := io.Copy(tmp, f); err != nil { // appended since: whole lines, as appends hold ledgerMu
		log.Printf("ledger gc: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		log.Printf("ledger gc: %v", err)
		return
	}
	done = true
	if err := os.Rename(tmpPath, path); err != nil {
		log.Printf("ledger gc: %v", err)
		os.Remove(tmpPath)
	}
}
