package netns

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"dsvs/internal/config"
)

const (
	// quiet is the silence that closes a burst of events: a VPN client sets its addresses in
	// several steps, a NetVM change recreates the interface then sets its route again, and a single
	// value must come out of all that. It must cover the gap between the link's removal and the new
	// route; to be tuned on a real measurement (ip -ts monitor link address route).
	quiet = time.Second

	// longest bounds the burst: a link that never stops moving does not hold back the observation
	// forever.
	longest = 5 * time.Second
)

// Observe follows the normal world's interfaces. The first value is the present state, read after
// subscribing, so no change falls between the two; the next ones come after each burst has settled,
// and only when they differ from the previous one. Empty ctl: this process is in the normal world
// and reads itself. Otherwise, the gatekeeper reads for it through the "links" request. The error
// only tells the failure to start; a read that breaks afterwards closes the channel and is told in
// line.
func Observe(ctx context.Context, ctl string, line func(string)) (<-chan config.Observed, error) {
	lost := func(reason string) { line("!! network no longer observed: " + reason) }
	if ctl == "" {
		return followLinks(ctx, lost)
	}
	return askLinks(ctx, ctl, lost)
}

// followLinks reads netlink right here. The channel closes at the end of the context, or when the
// read breaks, and lost then says why.
func followLinks(ctx context.Context, lost func(string)) (<-chan config.Observed, error) {
	ctx, cancel := context.WithCancel(ctx)
	var failure atomic.Pointer[error]
	remember := func(err error) { failure.Store(&err) }
	links := make(chan netlink.LinkUpdate, 64)
	addrs := make(chan netlink.AddrUpdate, 64)
	routes := make(chan netlink.RouteUpdate, 64)
	if err := netlink.LinkSubscribeWithOptions(links, ctx.Done(), netlink.LinkSubscribeOptions{ErrorCallback: remember}); err != nil {
		cancel()
		return nil, err
	}
	if err := netlink.AddrSubscribeWithOptions(addrs, ctx.Done(), netlink.AddrSubscribeOptions{ErrorCallback: remember}); err != nil {
		cancel()
		return nil, err
	}
	if err := netlink.RouteSubscribeWithOptions(routes, ctx.Done(), netlink.RouteSubscribeOptions{ErrorCallback: remember}); err != nil {
		cancel()
		return nil, err
	}
	seen, err := readNetwork()
	if err != nil {
		cancel()
		return nil, err
	}

	out := make(chan config.Observed, 1)
	out <- seen
	go func() {
		defer close(out)
		// The subscriptions are drained after us: a library that writes without looking at the
		// context would otherwise stay blocked on a channel nobody reads anymore.
		defer func() {
			cancel()
			go drain(links)
			go drain(addrs)
			go drain(routes)
		}()
		for {
			if !settled(links, addrs, routes) {
				break
			}
			next, err := readNetwork()
			if err != nil {
				remember(err)
				break
			}
			if reflect.DeepEqual(next, seen) {
				continue
			}
			seen = next
			select {
			case out <- seen:
			case <-ctx.Done():
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		reason := errors.New("the netlink subscription closed")
		if failed := failure.Load(); failed != nil {
			reason = *failed
		}
		lost(reason.Error())
	}()
	return out, nil
}

// settled waits for a change, then for it to calm down. False when a subscription closes.
func settled(links <-chan netlink.LinkUpdate, addrs <-chan netlink.AddrUpdate, routes <-chan netlink.RouteUpdate) bool {
	var calm, burst *time.Timer
	var calmed, bursted <-chan time.Time
	for {
		var open bool
		select {
		case _, open = <-links:
		case _, open = <-addrs:
		case _, open = <-routes:
		case <-calmed:
			return true
		case <-bursted:
			return true
		}
		if !open {
			return false
		}
		if calm == nil {
			calm, burst = time.NewTimer(quiet), time.NewTimer(longest)
			defer calm.Stop()
			defer burst.Stop()
			calmed, bursted = calm.C, burst.C
		} else {
			calm.Reset(quiet)
		}
	}
}

// readNetwork reads the whole state rather than applying the events one by one: the kernel removes
// a deleted link's default route without emitting an event, so only re-reading is right. A link is
// kept when the administrator brought it up, it has its carrier, it is not the loopback, and it
// carries an address of universal scope — that one alone is kept. The running flag counts, not the
// operational state: an up tun or wireguard commonly stays UNKNOWN, while a bridge without a port
// has no carrier.
func readNetwork() (config.Observed, error) {
	all, err := netlink.LinkList()
	if err != nil {
		return config.Observed{}, err
	}
	addrs, err := netlink.AddrList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return config.Observed{}, err
	}
	seen := config.Observed{Links: []config.Link{}, Exits: []config.Exit{}}
	for _, link := range all {
		attrs := link.Attrs()
		if attrs.Flags&net.FlagRunning == 0 || attrs.Flags&net.FlagLoopback != 0 {
			continue
		}
		held := []string{}
		for _, addr := range addrs {
			if addr.LinkIndex == attrs.Index && addr.Scope == int(netlink.SCOPE_UNIVERSE) {
				held = append(held, addr.IPNet.String())
			}
		}
		if len(held) > 0 {
			slices.Sort(held)
			seen.Links = append(seen.Links, config.Link{Name: attrs.Name, Type: link.Type(), Addrs: held})
		}
	}
	slices.SortFunc(seen.Links, func(a, b config.Link) int { return strings.Compare(a.Name, b.Name) })
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		found, err := netlink.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return config.Observed{}, err
		}
		found = slices.DeleteFunc(found, func(route netlink.Route) bool { return !isDefault(route.Dst) })
		slices.SortStableFunc(found, func(a, b netlink.Route) int { return a.Priority - b.Priority })
		for _, route := range found {
			exit := config.Exit{}
			if route.Gw != nil {
				exit.Via = route.Gw.String()
			}
			if i := slices.IndexFunc(all, func(link netlink.Link) bool { return link.Attrs().Index == route.LinkIndex }); i >= 0 {
				exit.Dev = all[i].Attrs().Name
			}
			seen.Exits = append(seen.Exits, exit)
		}
	}
	return seen, nil
}

// isDefault: the kernel returns the default route with a nil or 0.0.0.0/0 destination.
func isDefault(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	ones, _ := dst.Mask.Size()
	return ones == 0
}

func drain[T any](updates <-chan T) {
	for range updates {
	}
}

// askLinks asks the gatekeeper, which lives in the normal world, to read for us: one JSON line per
// value, or "err <reason>".
func askLinks(ctx context.Context, ctl string, lost func(string)) (<-chan config.Observed, error) {
	conn, err := net.Dial("unix", ctl)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintln(conn, "links"); err != nil {
		conn.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	lines := bufio.NewScanner(conn)
	first, err := readObserved(lines)
	if err != nil {
		stop()
		conn.Close()
		return nil, err
	}

	out := make(chan config.Observed, 1)
	out <- first
	go func() {
		defer close(out)
		defer conn.Close()
		for {
			seen, err := readObserved(lines)
			if err != nil {
				if ctx.Err() == nil {
					lost(err.Error())
				}
				return
			}
			select {
			case out <- seen:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func readObserved(lines *bufio.Scanner) (config.Observed, error) {
	var seen config.Observed
	if !lines.Scan() {
		if err := lines.Err(); err != nil {
			return seen, err
		}
		return seen, io.ErrUnexpectedEOF
	}
	if reason, refused := strings.CutPrefix(lines.Text(), "err "); refused {
		return seen, errors.New(reason)
	}
	return seen, json.Unmarshal(lines.Bytes(), &seen)
}

// tellLinks serves the "links" request: it reads for the supervisor, which cannot, and writes it
// each value until it hangs up.
func tellLinks(conn net.Conn) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		cancel()
	}()
	values, err := followLinks(ctx, func(reason string) { fmt.Fprintln(conn, "err "+reason) })
	if err != nil {
		return err
	}
	for seen := range values {
		text, err := json.Marshal(seen)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(conn, "%s\n", text); err != nil {
			return nil
		}
	}
	return nil
}
