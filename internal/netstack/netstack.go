//go:build linux

// Package netstack gives a container a network that lives on the client: a TUN device feeds a
// user-space TCP/IP stack (gVisor), and every TCP connection or UDP flow it terminates is handed
// to a Dialer, which opens it from the client's machine. The job then sees the client's egress
// IP, DNS, LAN and tailnet, and reaches the client's own localhost through ClientIP.
package netstack

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"strconv"
	"time"

	"github.com/RestartFU/offload/internal/streamio"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Addresses inside the container. ClientIP stands for the client machine: port 53 is its
// resolver, every other port its localhost.
var (
	LocalIP  = netip.MustParseAddr("10.233.0.2")
	ClientIP = netip.MustParseAddr("10.233.0.1")
)

const mtu = 1500

// Dialer opens a connection from the client's side. proto is "tcp" or "udp"; for UDP the returned
// stream carries datagrams framed with a 2-byte big-endian length.
type Dialer func(proto, addr string) (io.ReadWriteCloser, error)

// Start creates tun0, routes everything through it and serves it until the process exits.
func Start(dial Dialer) error {
	fd, err := openTUN("tun0")
	if err != nil {
		return fmt.Errorf("tun: %w", err)
	}
	if err := configure("tun0", LocalIP, netip.MustParseAddr("255.255.255.255")); err != nil {
		return fmt.Errorf("configure tun0: %w", err)
	}
	if err := addDefaultRoute("tun0"); err != nil {
		return fmt.Errorf("default route: %w", err)
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	ep, err := fdbased.New(&fdbased.Options{FDs: []int{fd}, MTU: mtu})
	if err != nil {
		return fmt.Errorf("link: %w", err)
	}
	const nic = 1
	if terr := s.CreateNIC(nic, ep); terr != nil {
		return fmt.Errorf("nic: %s", terr)
	}
	// Accept every destination: the stack impersonates whatever the program connects to.
	s.SetPromiscuousMode(nic, true)
	s.SetSpoofing(nic, true)
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nic}})

	tcpFwd := tcp.NewForwarder(s, 0, 2048, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		addr := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))
		up, err := dial("tcp", addr)
		if err != nil {
			r.Complete(true) // refuse, like the real destination would
			return
		}
		var wq waiter.Queue
		cep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			r.Complete(true)
			up.Close()
			return
		}
		r.Complete(false)
		go streamio.Splice(gonet.NewTCPConn(&wq, cep), up)
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	udpFwd := udp.NewForwarder(s, func(r *udp.ForwarderRequest) bool {
		id := r.ID()
		addr := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))
		var wq waiter.Queue
		cep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			return false
		}
		go relayUDP(gonet.NewUDPConn(&wq, cep), addr, dial)
		return true
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)
	return nil
}

// relayUDP carries one UDP flow (one source, one destination) over a framed stream, until it has
// been idle for two minutes.
func relayUDP(c *gonet.UDPConn, addr string, dial Dialer) {
	defer c.Close()
	up, err := dial("udp", addr)
	if err != nil {
		return
	}
	defer up.Close()
	go func() {
		var hdr [2]byte
		buf := make([]byte, 65535)
		for {
			if _, err := io.ReadFull(up, hdr[:]); err != nil {
				c.Close()
				return
			}
			n := int(hdr[0])<<8 | int(hdr[1])
			if _, err := io.ReadFull(up, buf[:n]); err != nil {
				c.Close()
				return
			}
			c.Write(buf[:n])
		}
	}()
	buf := make([]byte, 65535)
	frame := make([]byte, 2+65535)
	for {
		c.SetReadDeadline(time.Now().Add(2 * time.Minute))
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		frame[0], frame[1] = byte(n>>8), byte(n)
		copy(frame[2:], buf[:n])
		if _, err := up.Write(frame[:2+n]); err != nil {
			return
		}
	}
}

func openTUN(name string) (int, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return -1, err
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func configure(name string, addr, mask netip.Addr) error {
	s, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(s)
	set := func(req uint, fill func(*unix.Ifreq) error) error {
		ifr, err := unix.NewIfreq(name)
		if err != nil {
			return err
		}
		if err := fill(ifr); err != nil {
			return err
		}
		return unix.IoctlIfreq(s, req, ifr)
	}
	if err := set(unix.SIOCSIFADDR, func(r *unix.Ifreq) error { return r.SetInet4Addr(addr.AsSlice()) }); err != nil {
		return fmt.Errorf("address: %w", err)
	}
	if err := set(unix.SIOCSIFNETMASK, func(r *unix.Ifreq) error { return r.SetInet4Addr(mask.AsSlice()) }); err != nil {
		return fmt.Errorf("netmask: %w", err)
	}
	if err := set(unix.SIOCSIFMTU, func(r *unix.Ifreq) error { r.SetUint32(mtu); return nil }); err != nil {
		return fmt.Errorf("mtu: %w", err)
	}
	return set(unix.SIOCSIFFLAGS, func(r *unix.Ifreq) error { r.SetUint16(unix.IFF_UP | unix.IFF_RUNNING); return nil })
}

// addDefaultRoute sends all IPv4 traffic to the interface (netlink RTM_NEWROUTE, 0.0.0.0/0 dev).
// The kernel adds no prefix route for a /0 address, so this has to be explicit.
func addDefaultRoute(name string) error {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// nlmsghdr (16) + rtmsg (12) + rtattr RTA_OIF (4 + 4)
	msg := make([]byte, 36)
	le := binary.NativeEndian
	le.PutUint32(msg[0:], uint32(len(msg)))
	le.PutUint16(msg[4:], unix.RTM_NEWROUTE)
	le.PutUint16(msg[6:], unix.NLM_F_REQUEST|unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)
	le.PutUint32(msg[8:], 1)     // seq
	msg[16] = unix.AF_INET       // family
	msg[17] = 0                  // dst_len: default route
	msg[20] = unix.RT_TABLE_MAIN // table
	msg[21] = unix.RTPROT_BOOT   // protocol
	msg[22] = unix.RT_SCOPE_LINK // scope: directly on the device
	msg[23] = unix.RTN_UNICAST   // type
	le.PutUint16(msg[28:], 8)    // rta_len
	le.PutUint16(msg[30:], unix.RTA_OIF)
	le.PutUint32(msg[32:], uint32(ifc.Index))
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	if n >= 20 && le.Uint16(buf[4:]) == unix.NLMSG_ERROR {
		if code := int32(le.Uint32(buf[16:])); code != 0 {
			return unix.Errno(-code)
		}
	}
	return nil
}

// WriteResolvConf points the container's DNS at the client's resolver.
func WriteResolvConf() {
	conf := "# offload: DNS is answered by the client machine's resolver\nnameserver " + ClientIP.String() + "\n"
	if search := os.Getenv("OFFLOAD_DNS_SEARCH"); search != "" {
		conf += "search " + search + "\n"
	}
	if err := os.WriteFile("/etc/resolv.conf", []byte(conf), 0o644); err != nil {
		log.Printf("offload: resolv.conf: %v", err)
	}
	f, err := os.OpenFile("/etc/hosts", os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		fmt.Fprintf(f, "%s\tlaptop client.offload\n", ClientIP)
		f.Close()
	}
}
