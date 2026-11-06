package core

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Astray says that a starting app listens, but not at its address: where it listens, and the
// address the network gave it. Nothing is guessed: dsvs reads the listening sockets that the run's
// process tree holds open, in the tables of the network it lives in.
type Astray struct {
	On   []string
	Want Endpoint
}

func (a Astray) String() string {
	return "listens on " + strings.Join(a.On, ", ") + ", not on " + a.Want.String()
}

// strayed returns where a run that does not listen at its address listens, and nil when it does
// listen there — a socket on all addresses of its port included — or when it listens nowhere yet.
func strayed(pid int, want Endpoint) *Astray {
	on := listening(pid)
	at, err := netip.ParseAddr(want.IP)
	if len(on) == 0 || err != nil {
		return nil
	}
	var elsewhere []string
	for _, addr := range on {
		if int(addr.Port()) == want.Port && (addr.Addr() == at || addr.Addr().IsUnspecified()) {
			return nil
		}
		elsewhere = append(elsewhere, addr.String())
	}
	return &Astray{On: elsewhere, Want: want}
}

// listening returns the addresses where this process or one of its descendants listens: the sockets
// they hold, looked up in the TCP tables of the process's network.
func listening(pid int) []netip.AddrPort {
	held := map[string]bool{}
	for _, one := range lineage(pid) {
		fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", one))
		for _, fd := range fds {
			link, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", one, fd.Name()))
			if inode, socket := strings.CutPrefix(link, "socket:["); socket {
				held[strings.TrimSuffix(inode, "]")] = true
			}
		}
	}
	var out []netip.AddrPort
	for _, table := range []string{"tcp", "tcp6"} {
		text, _ := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, table))
		for _, addr := range listeners(string(text), held) {
			if !slices.Contains(out, addr) {
				out = append(out, addr)
			}
		}
	}
	return out
}

// lineage returns this process and all its descendants, as the kernel lists them in children.
func lineage(pid int) []int {
	out := []int{pid}
	for i := 0; i < len(out); i++ {
		tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", out[i]))
		for _, task := range tasks {
			text, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", out[i], task.Name()))
			for _, field := range strings.Fields(string(text)) {
				if child, err := strconv.Atoi(field); err == nil {
					out = append(out, child)
				}
			}
		}
	}
	return out
}

// listeners reads a /proc/net/tcp or tcp6 table: the addresses of the listening sockets that held
// holds, by their inode. An IPv4 address seen through an IPv6 socket is reported as IPv4.
func listeners(table string, held map[string]bool) []netip.AddrPort {
	const listen = "0A"
	var out []netip.AddrPort
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[3] != listen || !held[fields[9]] {
			continue
		}
		if addr, ok := local(fields[1]); ok {
			out = append(out, addr)
		}
	}
	return out
}

// local reads an address from the table: the address as 32-bit words in host byte order, then the
// port in hexadecimal.
func local(text string) (netip.AddrPort, bool) {
	host, port, found := strings.Cut(text, ":")
	words, err := hex.DecodeString(host)
	number, perr := strconv.ParseUint(port, 16, 16)
	if !found || err != nil || perr != nil || (len(words) != 4 && len(words) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(words); i += 4 {
		binary.NativeEndian.PutUint32(words[i:], binary.BigEndian.Uint32(words[i:]))
	}
	addr, _ := netip.AddrFromSlice(words)
	return netip.AddrPortFrom(addr.Unmap(), uint16(number)), true
}
