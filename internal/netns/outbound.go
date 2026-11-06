package netns

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	mtu = 1500
	nic = 1

	// A UDP conversation has no end: it is closed when nothing goes through anymore.
	udpIdle = 30 * time.Second
)

// allowed says what is allowed out, and is called before each real connection: everything goes out,
// except the virtual network's private addresses — nothing legitimate looks for them outside, and
// following them there would leak the address plan onto the host.
func allowed(dst netip.AddrPort) bool {
	return !private.Contains(dst.Addr())
}

// private is set when the way out is set up, when the subnet is known; resolver and tun are set at
// the same moment. All three only concern the gatekeeper, which has only one way out to hold, and a
// negative tun says it is not connected yet.
var (
	private  netip.Prefix
	resolver Resolver
	tun      = -1
)

// relay connects the private network's TUN to a userspace network stack and returns: connections
// are handled as they come, each in its goroutine.
func relay(fd int) error {
	if err := unix.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("the TUN refuses non-blocking mode: %w", err)
	}
	tun = fd
	endpoint, err := fdbased.New(&fdbased.Options{FDs: []int{fd}, MTU: mtu})
	if err != nil {
		return fmt.Errorf("network stack: %w", err)
	}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	forwarder := tcp.NewForwarder(s, 0, 1024, func(r *tcp.ForwarderRequest) {
		// The destination is read before anything else: Complete invalidates the request.
		dst := where(r.ID())
		if dst.Port() == dnsPort {
			if conn, ok := accept(r); ok {
				go serveDNS(conn, true, resolver, watchers.line)
			}
			return
		}
		// The real connection opens before answering the app, and its failure is returned as a real
		// network would return it: a server that refuses, by a refusal; a destination out of reach,
		// by the ICMP a router would send. Accepting then closing used to blur them into a silent
		// end of connection, and an app — the session's proxy included — could no longer tell a
		// server that is down from a network that does not reach it.
		real, err := reach("tcp", dst)
		// After the dial: a SYN repeated while it was waiting is filed under the same key.
		syn, _ := syns.LoadAndDelete(r.ID())
		if err != nil {
			refused := errors.Is(err, unix.ECONNREFUSED) || errors.Is(err, errPrivate)
			r.Complete(refused)
			if quoted, ipv4 := syn.([]byte); ipv4 && !refused {
				unreachable(quoted, errors.Is(err, unix.ENETUNREACH))
			}
			return
		}
		conn, ok := accept(r)
		if !ok {
			real.Close()
			return
		}
		go pipe(conn, real, false)
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		if pkt.NetworkProtocolNumber == header.IPv4ProtocolNumber && header.TCP(pkt.TransportHeader().Slice()).Flags() == header.TCPFlagSyn {
			syns.Store(id, slices.Concat(pkt.NetworkHeader().Slice(), pkt.TransportHeader().Slice()[:8]))
		}
		return forwarder.HandlePacket(id, pkt)
	})

	datagrams := udp.NewForwarder(s, func(r *udp.ForwarderRequest) {
		dst := where(r.ID())
		var queue waiter.Queue
		endpoint, e := r.CreateEndpoint(&queue)
		if e != nil {
			return
		}
		virtual := gonet.NewUDPConn(&queue, endpoint)
		// DNS is not relayed, it is resolved, and so it does not go out: the address the app
		// targeted only exists in the private network, and it is the machine's stack that knows how
		// to answer.
		if dst.Port() == dnsPort {
			go serveDNS(virtual, false, resolver, watchers.line)
			return
		}
		real, err := reach("udp", dst)
		if err != nil {
			virtual.Close()
			return
		}
		go pipe(virtual, real, true)
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, datagrams.HandlePacket)

	// The interface is connected last: it delivers packets as soon as it is created, and they must
	// find the handlers in place.
	if e := s.CreateNIC(nic, endpoint); e != nil {
		return fmt.Errorf("stack interface: %v", e)
	}
	// The stack accepts everything that arrives, whatever the destination: that is what lets it
	// intercept connections not addressed to it.
	s.SetPromiscuousMode(nic, true)
	s.SetSpoofing(nic, true)
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nic},
		{Destination: header.IPv6EmptySubnet, NIC: nic},
	})
	return nil
}

// syns keeps, per connection waiting for its twin, the IPv4 header of its SYN and the start of its
// TCP header: it is what an ICMP must quote for the app's kernel to attach it to its socket. An
// IPv6 SYN is not there, and its out-of-reach destination is told by silence.
var syns sync.Map

// unreachable writes into the TUN what a router would answer to this SYN: host, or network,
// unreachable, on behalf of the destination. The app's connect then fails on EHOSTUNREACH or
// ENETUNREACH, right away, instead of waiting for a response that will not come.
func unreachable(quoted []byte, network bool) {
	original := header.IPv4(quoted)
	icmp := header.ICMPv4(make([]byte, header.ICMPv4MinimumSize+len(quoted)))
	icmp.SetType(header.ICMPv4DstUnreachable)
	icmp.SetCode(header.ICMPv4HostUnreachable)
	if network {
		icmp.SetCode(header.ICMPv4NetUnreachable)
	}
	copy(icmp[header.ICMPv4MinimumSize:], quoted)
	icmp.SetChecksum(^checksum.Checksum(icmp, 0))
	packet := header.IPv4(make([]byte, header.IPv4MinimumSize+len(icmp)))
	packet.Encode(&header.IPv4Fields{TotalLength: uint16(len(packet)), TTL: 64, Protocol: uint8(header.ICMPv4ProtocolNumber),
		SrcAddr: original.DestinationAddress(), DstAddr: original.SourceAddress()})
	packet.SetChecksum(^packet.CalculateChecksum())
	copy(packet[header.IPv4MinimumSize:], icmp)
	_, _ = unix.Write(tun, packet)
}

// accept completes the handshake with the app and returns its connection.
func accept(r *tcp.ForwarderRequest) (net.Conn, bool) {
	var queue waiter.Queue
	endpoint, e := r.CreateEndpoint(&queue)
	if e != nil {
		r.Complete(true)
		return nil, false
	}
	r.Complete(false)
	return gonet.NewTCPConn(&queue, endpoint), true
}

// errPrivate is the refusal of a virtual network address looked for outside.
var errPrivate = errors.New("refused")

// reach opens in the normal world the connection the app targeted, and tells how it went. The
// destination is the one the app targeted: neither the name, nor the certificate, nor anything of
// the content is touched.
func reach(network string, dst netip.AddrPort) (net.Conn, error) {
	if !allowed(dst) {
		watchers.line(fmt.Sprintf("%-4s %-45s refused", network, dst))
		return nil, errPrivate
	}
	real, err := net.DialTimeout(network, dst.String(), 10*time.Second)
	if err != nil {
		watchers.line(fmt.Sprintf("%-4s %-45s %v", network, dst, err))
		return nil, err
	}
	watchers.line(fmt.Sprintf("%-4s %-45s open", network, dst))
	return real, nil
}

// pipe copies the bytes both ways between an intercepted connection and its twin in the normal
// world.
func pipe(virtual, real net.Conn, datagram bool) {
	defer virtual.Close()
	defer real.Close()
	if datagram {
		// A datagram does not announce its end: without exchange, the conversation is over.
		_ = virtual.SetDeadline(time.Now().Add(udpIdle))
		_ = real.SetDeadline(time.Now().Add(udpIdle))
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyThenTell(real, virtual) }()
	go func() { defer wg.Done(); copyThenTell(virtual, real) }()
	wg.Wait()
}

// copyThenTell copies one direction of the conversation then tells the other end there will be
// nothing more: without this half-close, a client waiting for the end of the response would wait
// forever.
func copyThenTell(to, from net.Conn) {
	_, _ = io.Copy(to, from)
	if half, ok := to.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
		return
	}
	to.SetReadDeadline(time.Now())
}

// where returns the destination the app targeted: in an intercepted connection, it is the local
// address as the stack sees it.
func where(id stack.TransportEndpointID) netip.AddrPort {
	addr, _ := netip.AddrFromSlice(id.LocalAddress.AsSlice())
	return netip.AddrPortFrom(addr.Unmap(), id.LocalPort)
}

// watchers carries the connections through which the supervisor follows what goes out. The
// gatekeeper has no interface of its own: it tells, and it is the supervisor that logs.
var watchers = &audience{}

type audience struct {
	mu    sync.Mutex
	conns []net.Conn
}

// hold keeps a connection open until the other end closes it.
func (a *audience) hold(conn net.Conn) error {
	a.mu.Lock()
	a.conns = append(a.conns, conn)
	a.mu.Unlock()
	// Reading to the end keeps the connection open and tells when it closes.
	_, err := io.Copy(io.Discard, conn)
	a.drop(conn)
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (a *audience) drop(conn net.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, held := range a.conns {
		if held == conn {
			a.conns = append(a.conns[:i], a.conns[i+1:]...)
			return
		}
	}
}

// line sends a line to whoever listens. Nobody listening is a normal case: the gatekeeper also runs
// before the supervisor has opened its window.
func (a *audience) line(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, conn := range a.conns {
		_, _ = fmt.Fprintln(conn, strings.TrimRight(text, "\n"))
	}
}
