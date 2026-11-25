// Package streamio connects byte streams for client networking: it upgrades HTTP connections to
// raw streams without losing the underlying connection's CloseWrite, and splices two streams so
// a half-close (one side done sending, still reading) travels across instead of ending both.
package streamio

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Conn is a raw stream that may support half-close.
type Conn interface {
	io.ReadWriteCloser
}

type closeWriter interface{ CloseWrite() error }

// Buffered is a connection whose first bytes may already sit in a bufio.Reader (read together
// with the HTTP response or request). It keeps the underlying connection's CloseWrite.
type Buffered struct {
	net.Conn
	R *bufio.Reader
}

func (b Buffered) Read(p []byte) (int, error) { return b.R.Read(p) }

// CloseWrite half-closes the underlying connection when it supports it.
func (b Buffered) CloseWrite() error {
	if cw, ok := b.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return b.Conn.Close()
}

// Upgrade sends an HTTP/1.1 upgrade request for path over conn and, on 101, returns the raw
// stream. header may carry Authorization. On failure conn is closed.
func Upgrade(conn net.Conn, host, path string, header http.Header) (net.Conn, error) {
	req, err := http.NewRequest("POST", "http://"+host+path, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "offload-stream")
	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	_ = conn.SetDeadline(time.Time{})
	return Buffered{Conn: conn, R: br}, nil
}

// Splice copies both ways. When one direction ends (EOF), the other side is half-closed so it
// sees EOF too while its replies keep flowing back; both are closed once both directions end.
func Splice(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	half := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok && err == nil {
			if cw.CloseWrite() == nil {
				return
			}
		}
		// No half-close available, or the copy failed: tear both down.
		dst.Close()
		src.Close()
	}
	go half(a, b)
	go half(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// PacketConn is the datagram side of a UDP relay.
type PacketConn interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
}

// RelayDatagrams carries one UDP flow between a datagram connection and a stream of frames
// (2-byte big-endian length, then the payload) until either side ends, or until no datagram has
// moved in either direction for idle. Both are closed on return.
func RelayDatagrams(pkt PacketConn, stream io.ReadWriteCloser, idle time.Duration) {
	var last atomic.Int64
	touch := func() { last.Store(time.Now().UnixNano()) }
	touch()
	defer pkt.Close()
	defer stream.Close()
	go func() { // stream -> datagrams
		defer pkt.Close()
		r := bufio.NewReader(stream)
		var hdr [2]byte
		buf := make([]byte, 65535)
		for {
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				return
			}
			n := int(hdr[0])<<8 | int(hdr[1])
			if _, err := io.ReadFull(r, buf[:n]); err != nil {
				return
			}
			touch()
			if _, err := pkt.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 65535)
	frame := make([]byte, 2+65535)
	for { // datagrams -> stream
		_ = pkt.SetReadDeadline(time.Now().Add(min(idle, 15*time.Second)))
		n, err := pkt.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, last.Load())) < idle {
				continue // quiet this way, but the flow is alive in the other direction
			}
			return
		}
		touch()
		frame[0], frame[1] = byte(n>>8), byte(n)
		copy(frame[2:], buf[:n])
		if _, err := stream.Write(frame[:2+n]); err != nil {
			return
		}
	}
}
