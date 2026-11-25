package streamio

import (
	"io"
	"net"
	"testing"
	"time"
)

// A client that half-closes after its request must still get the server's reply through Splice.
func TestSpliceCarriesHalfClose(t *testing.T) {
	server, _ := net.Listen("tcp", "127.0.0.1:0")
	defer server.Close()
	go func() {
		c, err := server.Accept()
		if err != nil {
			return
		}
		req, _ := io.ReadAll(c) // reads until the client's half-close arrives
		c.Write(append([]byte("reply to "), req...))
		c.Close()
	}()
	front, _ := net.Listen("tcp", "127.0.0.1:0")
	defer front.Close()
	go func() {
		in, err := front.Accept()
		if err != nil {
			return
		}
		out, err := net.Dial("tcp", server.Addr().String())
		if err != nil {
			in.Close()
			return
		}
		Splice(in, out)
	}()
	c, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("ping"))
	c.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "reply to ping" {
		t.Fatalf("got %q, %v", got, err)
	}
}
