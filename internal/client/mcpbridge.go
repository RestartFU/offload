package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// mcpSession is one runner session behind the bridge.
type mcpSession interface {
	send(line []byte) error // a *remoteEnded when the session's input is gone
	closeInput() error
	ended() <-chan error // its stdout reached the end: the session is over
	finish(err error) error
	stop()
}

// reinitID is the id of the initialize request replayed to a restarted server. Its answer is
// the bridge's: the MCP client already has one.
const reinitID = `"offload-reinit"`

// bridge connects the MCP client on stdio to a runner session. MCP clients keep their servers
// running for as long as a conversation stays open, which can be hours after its last tool
// call. Once every request has been answered and none arrived for idle, the session is stopped
// to free what it holds on the runner. The next request starts a new one, which first gets the
// client's own initialize request and initialized notification again.
type bridge struct {
	idle time.Duration
	open func(stdout io.Writer) (mcpSession, error)
	out  io.Writer
	tick <-chan time.Time // when to look for idleness; derived from idle when nil

	outMu sync.Mutex

	mu                sync.Mutex
	pending           map[string]bool
	last              time.Time
	init, initialized []byte
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func parseMessage(line []byte) message {
	var m message
	_ = json.Unmarshal(line, &m)
	return m
}

func (m message) hasID() bool { return len(m.ID) > 0 && string(m.ID) != "null" }

func (m message) request() bool { return m.Method != "" && m.hasID() }

type input struct {
	line []byte
	err  error
}

func (b *bridge) run(ctx context.Context, in io.Reader) error {
	b.pending = map[string]bool{}
	cur, err := b.open(b.replies())
	if err != nil {
		return err
	}
	defer func() {
		if cur != nil {
			cur.stop()
		}
	}()
	lines := make(chan input)
	go readInput(ctx, in, lines)
	tick := b.tick
	if tick == nil && b.idle > 0 {
		t := time.NewTicker(max(min(b.idle/4, time.Minute), time.Millisecond))
		defer t.Stop()
		tick = t.C
	}
	for {
		var ended <-chan error
		if cur != nil {
			ended = cur.ended()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ended:
			return cur.finish(err)
		case <-tick:
			if cur != nil && b.idleFor() >= b.idle {
				fmt.Fprintf(os.Stderr, "offload: no requests for %s; stopped the session to free the runner, the next request starts a new one\n", b.idle)
				cur.stop()
				cur = nil
			}
		case l := <-lines:
			if l.err != nil {
				return b.inputEnded(ctx, cur, l.err)
			}
			m := parseMessage(l.line)
			if cur == nil {
				if m.Method == "ping" && m.hasID() {
					if err := b.emit(fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", m.ID)); err != nil {
						return err
					}
					continue
				}
				if !m.request() {
					continue // a notification or an answer meant for the stopped server
				}
				if cur, err = b.wake(); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					// Say why and stay up: the runner may take work again (a resume, free room).
					fmt.Fprintf(os.Stderr, "offload: couldn't start the session again: %v\n", err)
					msg, _ := json.Marshal("offload couldn't start the session again: " + err.Error())
					if err := b.emit(fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":%s}}`+"\n", m.ID, msg)); err != nil {
						return err
					}
					continue
				}
			}
			b.record(m, l.line)
			if err := cur.send(l.line); err != nil {
				return b.inputFailed(ctx, cur, err)
			}
		}
	}
}

func readInput(ctx context.Context, in io.Reader, lines chan<- input) {
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := readLineMax(r, maxMessage)
		if len(line) > 0 {
			select {
			case lines <- input{line: line}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			select {
			case lines <- input{err: err}:
			case <-ctx.Done():
			}
			return
		}
	}
}

func (b *bridge) inputEnded(ctx context.Context, cur mcpSession, err error) error {
	if errors.Is(err, errTooLong) {
		return fmt.Errorf("an MCP message is over the %d MiB a session accepts", maxMessage>>20)
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	if cur == nil {
		return nil
	}
	// The MCP client closed stdin, so the server's is closed after everything sent. Answers
	// still being worked on arrive until the server exits (or the caller gives up).
	if err := cur.closeInput(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-cur.ended():
		return cur.finish(err)
	}
}

func (b *bridge) inputFailed(ctx context.Context, cur mcpSession, err error) error {
	var ended *remoteEnded
	if !errors.As(err, &ended) {
		return err // a local problem: at once
	}
	// The server closed its input, often on its way out: its exit status is the real result,
	// if it arrives soon.
	select {
	case derr := <-cur.ended():
		if r := cur.finish(derr); derr == nil && (r == nil || errors.As(r, new(*ExitError))) {
			return r
		}
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
	}
	return err
}

func (b *bridge) wake() (mcpSession, error) {
	fmt.Fprintln(os.Stderr, "offload: a request arrived; starting the session again")
	s, err := b.open(b.replies())
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	replay := [][]byte{reinit(b.init), b.initialized}
	b.mu.Unlock()
	for _, line := range replay {
		if line == nil {
			continue
		}
		if err := s.send(line); err != nil {
			s.stop()
			return nil, err
		}
	}
	return s, nil
}

func reinit(line []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	m["id"] = json.RawMessage(reinitID)
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return append(out, '\n')
}

func (b *bridge) record(m message, line []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch m.Method {
	case "initialize":
		b.init = line
	case "notifications/initialized":
		b.initialized = line
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			delete(b.pending, string(p.RequestID))
		}
	}
	if m.request() {
		b.pending[string(m.ID)] = true
	}
	if m.Method != "ping" {
		b.last = time.Now()
	}
}

// fromServer notes an answer from the server and says whether the MCP client should see it.
func (b *bridge) fromServer(line []byte) bool {
	m := parseMessage(line)
	if m.Method != "" || !m.hasID() {
		return true
	}
	if string(m.ID) == reinitID {
		return false
	}
	b.mu.Lock()
	if b.pending[string(m.ID)] {
		delete(b.pending, string(m.ID))
		b.last = time.Now()
	}
	b.mu.Unlock()
	return true
}

func (b *bridge) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.init == nil || len(b.pending) > 0 {
		return 0 // not an MCP conversation the bridge can restart, or a request is in flight
	}
	return time.Since(b.last)
}

func (b *bridge) emit(line []byte) error {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, err := b.out.Write(line)
	return err
}

func (b *bridge) replies() io.Writer { return &replyWriter{b: b} }

// replyWriter passes one session's stdout to the MCP client a whole line at a time.
type replyWriter struct {
	b   *bridge
	buf []byte
}

func (w *replyWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := w.buf[:i+1]
		w.buf = w.buf[i+1:]
		if w.b.fromServer(line) {
			if err := w.b.emit(line); err != nil {
				return 0, err
			}
		}
	}
}
