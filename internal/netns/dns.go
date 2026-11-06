package netns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	resolvConf = "/etc/resolv.conf"
	dnsPort    = 53
	dnsTimeout = 5 * time.Second

	// A DNS message over UDP fits in a size announced by the client; this one covers EDNS0.
	dnsMax = 4096
)

// Resolver answers a DNS query coming from the private network. The message stays raw: the default
// resolution forwards it to the machine's resolver, which covers every record type and a VPN's
// internal names without understanding any of it. A resolver that wants to answer itself (synthetic
// addresses, per-app override) only has to read what it needs.
type Resolver func(ctx context.Context, query []byte) ([]byte, error)

// forwardTo returns a resolver that forwards the query to the machine's servers, the first to
// answer winning. It is the only place that talks to a real DNS server.
func forwardTo(servers []netip.AddrPort) Resolver {
	return func(ctx context.Context, query []byte) ([]byte, error) {
		if len(servers) == 0 {
			return nil, errors.New("no DNS server in " + resolvConf)
		}
		var problems []error
		for _, server := range servers {
			answer, err := ask(ctx, server, query)
			if err == nil {
				return answer, nil
			}
			problems = append(problems, err)
		}
		return nil, errors.Join(problems...)
	}
}

func ask(ctx context.Context, server netip.AddrPort, query []byte) ([]byte, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", server.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	answer := make([]byte, dnsMax)
	n, err := conn.Read(answer)
	if err != nil {
		return nil, err
	}
	return answer[:n], nil
}

// serveDNS answers the queries of a conversation intercepted on port 53. Over UDP each message is a
// datagram; over TCP it is preceded by its length on two bytes.
func serveDNS(conn net.Conn, stream bool, resolve Resolver, log func(string)) {
	_ = conn.SetDeadline(time.Now().Add(dnsTimeout))
	for {
		query, err := readQuery(conn, stream)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
		answer, err := resolve(ctx, query)
		cancel()
		if err != nil {
			log(fmt.Sprintf("%-4s %-45s %v", "dns", "resolve", err))
			return
		}
		if err := writeAnswer(conn, stream, answer); err != nil {
			return
		}
	}
}

func readQuery(conn net.Conn, stream bool) ([]byte, error) {
	if !stream {
		buf := make([]byte, dnsMax)
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, err
	}
	query := make([]byte, int(size[0])<<8|int(size[1]))
	if _, err := io.ReadFull(conn, query); err != nil {
		return nil, err
	}
	return query, nil
}

func writeAnswer(conn net.Conn, stream bool, answer []byte) error {
	if stream {
		size := []byte{byte(len(answer) >> 8), byte(len(answer))}
		answer = append(size, answer...)
	}
	_, err := conn.Write(answer)
	return err
}

// nameservers reads the servers the machine uses. They can be on loopback (systemd-resolved listens
// on 127.0.0.53): the gatekeeper lives in the host's network, so they are reachable for it.
func nameservers() []netip.AddrPort {
	data, err := os.ReadFile(resolvConf)
	if err != nil {
		return nil
	}
	return parseNameservers(data)
}

func parseNameservers(data []byte) []netip.AddrPort {
	var out []netip.AddrPort
	for _, line := range strings.Split(string(data), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		if key != "nameserver" {
			continue
		}
		if addr, err := netip.ParseAddr(strings.TrimSpace(value)); err == nil {
			out = append(out, netip.AddrPortFrom(addr, dnsPort))
		}
	}
	return out
}

// privateResolv makes the apps see a resolv.conf that designates the inside of the network: their
// queries therefore go into the TUN, whatever the machine wrote in its own. Without it, a
// resolv.conf on 127.0.0.53 would send the queries to the namespace's lo, where nobody listens.
//
// The search, domain and options lines are copied: they are what completes an internal domain's
// short names.
func privateResolv(dir string, gateway netip.Addr) error {
	var private strings.Builder
	fmt.Fprintf(&private, "nameserver %s\n", gateway)
	if host, err := os.ReadFile(resolvConf); err == nil {
		for _, line := range strings.Split(string(host), "\n") {
			switch key, _, _ := strings.Cut(strings.TrimSpace(line), " "); key {
			case "search", "domain", "options":
				private.WriteString(strings.TrimSpace(line) + "\n")
			}
		}
	}

	path := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(path, []byte(private.String()), 0o644); err != nil {
		return err
	}
	// The mount must not propagate up to the host: the namespace's root goes private first.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("private root: %w", err)
	}
	if err := unix.Mount(path, resolvConf, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("mounting %s: %w", resolvConf, err)
	}
	return nil
}
