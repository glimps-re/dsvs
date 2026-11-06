// Package hub holds the ports, for all sessions at once. A session no longer binds anything: it
// registers with the hub with the port and the hosts it serves, and receives the connections meant
// for them — the accepted socket itself, headers unread, so that the session's gateway sees an
// ordinary connection and knows nothing of this mechanism.
//
// The hub is the same binary, launched with --hub. It is born from the first one that needs it and
// dies with the last session: nobody has a daemon to start, or to stop. That is what lets two
// worktrees serve their hosts on the same port, and what makes an idle workstation hold no port —
// the room stays free for another tool.
//
// # What holds the shape
//
// A session is identified by its configuration's path, and by nothing else: two sessions for the
// same configuration is a state the hub refuses, and that is the name it gives in a refusal to say
// who holds the contested host. A host belongs to the first session that declares it — one rule,
// one guardian, held under the table's lock, so nobody downstream has any tie to break. The refusal
// is outright at registration; on a reload, it is news told to the session, which keeps running and
// keeps the rest of its hosts.
//
// A host nobody claims goes to the session that declares a facade without a host, and failing that
// gets a 503 that lists those being served. On the wire this catch-all carries a name, so that "no
// facade" and "take everything" are two different lines instead of the same silence.
//
// The port is not an identity, it is a resource: opened for the first session that claims it,
// closed with the last, and belonging to nobody in between. A registered session changes it without
// unregistering — unregistering to register again would let a hub it was the last of die, and
// someone else take its name during the trip: the hub checks the new port like a registration,
// under the same lock, and releases the old one if it serves nobody anymore; refused, the session
// stays where it is. The connections already passed are the session's, and it closes those of the
// old port: a tab that kept its own there is no longer served. A concurrent birth produces only one
// hub, the file lock that grants the right to hold the socket breaking the tie between the
// contenders.
//
// # Known limits, and their numbers
//
// The session is chosen when the connection opens, not on each request: a client that kept the same
// connection for another host would stay with the session it was handed to. A browser does not do
// that — its pool is per origin — but an HTTP client that reuses its connections per address does.
//
// The delays all live in a constant of this package, and each says why it exists where it is
// declared. Headers of more than 4096 bytes, or that take more than peekPatience to carry their
// Host, get a 400; what is not an HTTP request gets its own from the first segment. A session that
// does not take within passPatience the connection handed to it is treated as gone: its control
// socket is closed, which unregisters it, and the connection goes back out as a 503. A host
// declaration the hub does not swallow within declarePatience drops the registration, which the
// port loop takes up again. Two revivals are at least reviveEvery apart, the first one not waiting
// for it. And a session that finds no hub in join keeps its apps and makes its port silent: nothing
// that runs is lost, but nothing answers there anymore.
//
// A hub of another version refuses the sessions, and says so naming both versions. It is the only
// scenario where an update costs a session its apps.
//
// Bind returns the socket and the lock together, and that is also what carries them across a
// handover's exec: Fds names them, Adopt takes them back. Releasing the lock for the time of a
// handover, even a few milliseconds, would let a client knock on nothing and launch a second
// session for the same configuration — it would take the name, and the new one would exit on
// ErrTaken, letting the cgroup take the apps down. The hub, for its part, leaves with its last
// session and the port reopens behind it: a handover therefore costs the in-flight connections to
// that port, for the time of the exec.
//
// Without a systemd session, a session in a private network refuses to launch the hub: a mere
// detach does not leave the network namespace, and the port would be invisible from the machine
// there. The message says so and suggests --net loopback.
package hub
