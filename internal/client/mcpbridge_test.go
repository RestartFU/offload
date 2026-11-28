package client

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSession struct {
	w       io.Writer
	got     chan string
	end     chan error
	stopped chan struct{}
	once    sync.Once
}

func (f *fakeSession) send(line []byte) error { f.got <- strings.TrimSpace(string(line)); return nil }
func (f *fakeSession) closeInput() error      { return nil }
func (f *fakeSession) ended() <-chan error    { return f.end }
func (f *fakeSession) finish(err error) error { return err }
func (f *fakeSession) stop()                  { f.once.Do(func() { close(f.stopped) }) }
func (f *fakeSession) reply(t *testing.T, s string) {
	t.Helper()
	if _, err := f.w.Write([]byte(s + "\n")); err != nil {
		t.Fatal(err)
	}
}

type lineSink struct{ lines chan string }

func (l *lineSink) Write(p []byte) (int, error) {
	l.lines <- strings.TrimSpace(string(p))
	return len(p), nil
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		panic("unreachable")
	}
}

func TestBridgeStopsIdleSessionAndRestartsIt(t *testing.T) {
	inR, inW := io.Pipe()
	out := &lineSink{lines: make(chan string, 16)}
	opened := make(chan *fakeSession, 2)
	tick := make(chan time.Time)
	b := &bridge{idle: time.Nanosecond, tick: tick, out: out, open: func(w io.Writer) (mcpSession, error) {
		f := &fakeSession{w: w, got: make(chan string, 16), end: make(chan error, 1), stopped: make(chan struct{})}
		opened <- f
		return f, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.run(ctx, inR) }()
	send := func(s string) {
		t.Helper()
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(ch chan string, want string) {
		t.Helper()
		if got := recv(t, ch); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	stopped := func(f *fakeSession) bool {
		select {
		case <-f.stopped:
			return true
		default:
			return false
		}
	}

	first := recv(t, opened)
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
	initialized := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	send(initialize)
	expect(first.got, initialize)
	first.reply(t, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	expect(out.lines, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	send(initialized)
	expect(first.got, initialized)
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"screenshot"}}`
	send(call)
	expect(first.got, call)

	tick <- time.Now()
	tick <- time.Now() // the first tick has been handled once the loop takes the second
	if stopped(first) {
		t.Fatal("stopped a session with a request in flight")
	}
	first.reply(t, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`)
	expect(out.lines, `{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`)
	tick <- time.Now()
	recv(t, first.stopped)

	send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	expect(out.lines, `{"jsonrpc":"2.0","id":3,"result":{}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"screenshot"}}`)
	second := recv(t, opened)
	var re struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(recv(t, second.got)), &re); err != nil {
		t.Fatal(err)
	}
	if string(re.ID) != reinitID || re.Method != "initialize" || string(re.Params) != `{"protocolVersion":"2025-06-18"}` {
		t.Fatalf("replayed %s %s %s, want the client's initialize under id %s", re.ID, re.Method, re.Params, reinitID)
	}
	second.reply(t, `{"jsonrpc":"2.0","id":"offload-reinit","result":{}}`)
	expect(second.got, initialized)
	expect(second.got, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"screenshot"}}`)
	second.reply(t, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`)
	expect(out.lines, `{"jsonrpc":"2.0","id":4,"result":{"content":[]}}`) // the replay's answer never reached the client

	inW.Close()
	second.end <- nil
	if err := recv(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeNeverStopsWithoutInitialize(t *testing.T) {
	b := &bridge{pending: map[string]bool{}}
	b.record(parseMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), nil)
	b.fromServer([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	if d := b.idleFor(); d != 0 {
		t.Fatalf("idle for %s without an initialize to replay", d)
	}
}
