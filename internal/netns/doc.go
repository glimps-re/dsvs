// Package netns gives the apps a private network: they listen on virtual addresses, in a network
// namespace nothing else sees, and the only ports open on the machine are the ingresses'. It is the
// default mode, and it requires no privilege.
//
// One binary, two roles. The gatekeeper stays in the normal world: it holds the public ports and
// the network stack that takes the traffic out. The supervisor is the same binary re-executed
// inside the namespace: it sets up the internal network and runs everything else — apps, proxy,
// interface. They talk over a Unix socket, which crosses namespaces because it is a file, and
// through which file descriptors travel.
//
// # DNS
//
// The apps see a resolv.conf that points inside, so their queries go up into the TUN like the rest,
// and it is the gatekeeper that resolves them — from the normal world, where the machine's whole
// resolution stack lives, VPN included.
//
// # The way out
//
// The packets the apps send outside go up into the TUN, are handled by a userspace TCP/IP stack
// held by the gatekeeper, and each connection is replayed by a real dial in the normal world: the
// machine's VPN, routes and system proxy therefore apply without anything having to know about
// them. TLS crosses intact — nobody terminates it, bytes are only copied.
//
// # .localhost names
//
// Measured, the limit of what browsers and curl resolve on their own: getaddrinfo fails on these
// names (Errno -2, nsswitch without myhostname), so a node script that targets such a name fails
// where the browser succeeds.
//
// # The handover
//
// A session that moves to the binary on disk moves its gatekeeper there first, through a reload on
// the control socket: it execs itself like the supervisor, keeps its PID — hence its systemd unit,
// its cgroup and its child — and carries across the TUN, the control socket, the subnet and the PID
// to wait for. The gatekeeper's refusal stops the handover before the session has dismantled
// anything.
//
// What its exec does not keep: the outgoing connections in flight in the userspace stack, replayed
// by a dial in the normal world, drop — the same family as the proxy's in-flight connections at the
// session's handover, and already accepted there. The Watch connection drops too, and the
// supervisor's new image reopens it.
//
// Pdeathsig on the supervisor and the handover exclude each other: the signal fires at the parent's
// exec (measured), and the supervisor would die of the gatekeeper's. The gatekeeper therefore only
// sets it outside the systemd box, and refuses the reload where it set it — the box's cgroup is
// what replaces it.
//
// # Looking inside
//
// From outside, nsenter -t <supervisor pid> -U -n -m --preserve-credentials. The -U is not
// optional: without it, the user namespace's uid mappings do not apply and the rest fails.
//
// # Ruled out, and why
//
// A PID namespace on top of the user and network ones: nx indexes its invocations on the tree's
// root pid, and in a new namespace two tasks get the same one — the second dies on "Recursive task
// invocation detected" (measured, two dev-servers together). The systemd box gives the kernel's
// guarantee against orphans without renumbering pids or playing init.
package netns
