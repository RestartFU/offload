package runner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RestartFU/offload/internal/streamio"
)

// Client networking: a job or session with network = "client" runs in its own network namespace,
// where `offload runner inner` terminates every connection in a user-space stack and asks the
// runner to dial it (POST /v1/{jobs|sessions}/{id}/dial, local socket only). The runner asks the
// attached client through the relay; the client dials the destination from its own machine and
// connects back (POST /v1/streams/{sid}); the runner then splices the two connections. Both legs
// are HTTP/1.1 upgrades to a raw byte stream.

// pendingStream is one dial waiting for the client's leg. The leg is delivered at most once; the
// dial claims it, or whoever ends the pairing closes it.
type pendingStream struct {
	conn  net.Conn
	ready chan struct{}
}

type streamHub struct {
	mu      sync.Mutex
	pending map[string]*pendingStream
}

var hub = &streamHub{pending: map[string]*pendingStream{}}

func (h *streamHub) expect(id string) *pendingStream {
	p := &pendingStream{ready: make(chan struct{})}
	h.mu.Lock()
	h.pending[id] = p
	h.mu.Unlock()
	return p
}

// claim ends a pairing and returns the delivered leg, if any. Called exactly once per dial.
func (h *streamHub) claim(id string) net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.pending[id]
	delete(h.pending, id)
	if p == nil {
		return nil
	}
	c := p.conn
	p.conn = nil
	return c
}

// deliver hands the client's leg to a waiting dial; false (and the caller closes c) if there is
// no such dial or it already has a leg.
func (h *streamHub) deliver(id string, c net.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.pending[id]
	if p == nil || p.conn != nil {
		return false
	}
	p.conn = c
	close(p.ready)
	return true
}

// upgrade hijacks the request's connection and answers 101, leaving a raw stream. The handshake
// has a deadline, and the connection is closed on any error.
func upgrade(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("connection can't be upgraded")
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: offload-stream\r\nConnection: Upgrade\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return c, rw, nil
}

// dialRequest is what the client receives (callback kind "dial").
type dialRequest struct {
	Stream string `json:"stream"`
	Proto  string `json:"proto"` // "tcp" or "udp" (framed datagrams)
	Addr   string `json:"addr"`
}

// handleDial runs a connection from inside a container through the client.
func (s *Server) handleDial(w http.ResponseWriter, r *http.Request) {
	if r.Context().Value(unixMarker{}) == nil {
		writeErr(w, 403, "local socket only")
		return
	}
	o := s.relayOf(w, r)
	if o == nil {
		return
	}
	if !o.allowDial {
		writeErr(w, 403, "this workload does not use client networking")
		return
	}
	req := dialRequest{Stream: newID("st-"), Proto: r.URL.Query().Get("proto"), Addr: r.URL.Query().Get("addr")}
	if req.Proto != "tcp" && req.Proto != "udp" {
		writeErr(w, 400, "proto must be tcp or udp")
		return
	}
	if _, _, err := net.SplitHostPort(req.Addr); err != nil {
		writeErr(w, 400, "bad addr")
		return
	}
	pending := hub.expect(req.Stream)
	var remote net.Conn
	defer func() {
		if c := hub.claim(req.Stream); c != nil {
			c.Close() // delivered but never used (the dial gave up)
		}
	}()
	body, _ := json.Marshal(req)
	answer, err := o.rl.ask("dial", body, 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if a := strings.TrimSpace(string(answer)); a != "ok" {
		writeErr(w, http.StatusBadGateway, a)
		return
	}
	select {
	case <-pending.ready:
		remote = hub.claim(req.Stream)
	case <-time.After(15 * time.Second):
		writeErr(w, http.StatusGatewayTimeout, "client did not connect the stream")
		return
	}
	if remote == nil {
		writeErr(w, http.StatusBadGateway, "stream vanished")
		return
	}
	local, rw, err := upgrade(w)
	if err != nil {
		remote.Close()
		return
	}
	leg := streamio.Buffered{Conn: local, R: rw.Reader}
	// Tracked on the workload's relay: when the job or session ends, its streams are closed.
	if !o.rl.track(leg, remote) {
		leg.Close()
		remote.Close()
		return
	}
	defer o.rl.untrack(leg, remote)
	streamio.Splice(leg, remote)
}

// handleStream is the client's leg of a dial: it is paired with the waiting container leg.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sid")
	hub.mu.Lock()
	p, ok := hub.pending[id]
	ok = ok && p.conn == nil
	hub.mu.Unlock()
	if !ok {
		writeErr(w, 404, "no such stream")
		return
	}
	c, rw, err := upgrade(w)
	if err != nil {
		return
	}
	if !hub.deliver(id, streamio.Buffered{Conn: c, R: rw.Reader}) {
		c.Close()
	}
}
