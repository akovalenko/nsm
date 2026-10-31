// Package expose implements the mount protocol: a veth pair between main and
// a connection's namespace, explicit routes in main for the exposed
// prefixes, double NAT so neither side needs to know the other's addressing,
// and loud refusal when a prefix collides with what main already has.
package expose

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"github.com/akovalenko/nsm/internal/conn"
	"github.com/akovalenko/nsm/internal/netns"
	"github.com/akovalenko/nsm/internal/sys"
)

// NSVeth is the namespace-side veth name; it is the same in every ns.
const NSVeth = "ve0"

// Result is the fate of one exposed prefix.
type Result struct {
	Prefix   netip.Prefix
	Mounted  bool
	Rejected bool   // refused on purpose (collision), as opposed to merely absent
	Reason   string // why not, when !Mounted
}

func (r Result) String() string {
	switch {
	case r.Mounted:
		return r.Prefix.String() + " mounted"
	case r.Rejected:
		return r.Prefix.String() + " REJECTED: " + r.Reason
	}
	return r.Prefix.String() + " not mounted: " + r.Reason
}

// Pair is the /31 a connection's veth got from the pool.
type Pair struct {
	Main netip.Addr // main side (ve-<name>)
	NS   netip.Addr // namespace side (ve0)
}

// route is the subset of `ip -j route` nsm reads.
type route struct {
	Dst      string `json:"dst"`
	Dev      string `json:"dev"`
	Via      string `json:"gateway"`
	Protocol string `json:"protocol"`
}

func (r route) prefix() (netip.Prefix, bool) {
	if r.Dst == "default" {
		return netip.Prefix{}, false
	}
	if p, err := netip.ParsePrefix(r.Dst); err == nil {
		return p, true
	}
	if a, err := netip.ParseAddr(r.Dst); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

func mainRoutes(s *sys.Sys) ([]route, error) {
	out, err := s.IPQ(sys.Main, "-j", "-4", "route", "show", "table", "main")
	if err != nil {
		return nil, err
	}
	var rs []route
	if out == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(out), &rs); err != nil {
		return nil, fmt.Errorf("ip -j route: %w", err)
	}
	return rs, nil
}

func overlaps(a, b netip.Prefix) bool { return a.Contains(b.Addr()) || b.Contains(a.Addr()) }

// Check decides whether prefix p may be mounted through veth given main's
// routes. Routes already pointing at our own veth never collide (they are
// earlier mounts of ours); anything else that overlaps is a refusal.
func Check(p netip.Prefix, routes []route, veth string) Result {
	for _, r := range routes {
		rp, ok := r.prefix()
		if !ok || r.Dev == veth || !overlaps(p, rp) {
			continue
		}
		reason := fmt.Sprintf("overlaps %s dev %s", rp, r.Dev)
		if strings.HasPrefix(r.Dev, "ve-") {
			reason += " (mounted by " + strings.TrimPrefix(r.Dev, "ve-") + ")"
		}
		return Result{Prefix: p, Rejected: true, Reason: reason}
	}
	return Result{Prefix: p, Mounted: true}
}

// allocPair finds the /31 for c: the one its veth already has, else the
// first free one in pool, judging by the addresses present in main.
func allocPair(s *sys.Sys, c *conn.Conn, pool netip.Prefix) (Pair, error) {
	links, err := netns.Links(s, sys.Main)
	if err != nil {
		return Pair{}, err
	}
	used := map[netip.Addr]bool{}
	for _, l := range links {
		for _, a := range l.AddrInfo {
			addr, err := netip.ParseAddr(a.Local)
			if err != nil || !pool.Contains(addr) {
				continue
			}
			if l.Ifname == c.Veth() {
				return pairAt(addr), nil
			}
			used[pairAt(addr).Main] = true
		}
	}
	for a := pool.Addr(); pool.Contains(a); a = a.Next().Next() {
		if !used[a] {
			return pairAt(a), nil
		}
	}
	return Pair{}, fmt.Errorf("veth pool %s exhausted", pool)
}

// pairAt returns the /31 containing addr (even address = main side).
func pairAt(addr netip.Addr) Pair {
	b := addr.As4()
	b[3] &^= 1
	m := netip.AddrFrom4(b)
	return Pair{Main: m, NS: m.Next()}
}

func ensureVeth(s *sys.Sys, c *conn.Conn, pool netip.Prefix) (Pair, error) {
	ns, veth := c.NS(), c.Veth()
	if !netns.LinkExists(s, sys.Main, veth) {
		// A stale ns-side half without its main half cannot be reused.
		s.TryIP(ns, "link", "del", NSVeth)
		if _, err := s.IP(sys.Main, "link", "add", veth, "type", "veth", "peer", "name", NSVeth, "netns", ns); err != nil {
			return Pair{}, err
		}
	}
	pair, err := allocPair(s, c, pool)
	if err != nil {
		return Pair{}, err
	}
	if _, err := s.IP(sys.Main, "addr", "replace", pair.Main.String()+"/31", "dev", veth); err != nil {
		return Pair{}, err
	}
	if _, err := s.IP(ns, "addr", "replace", pair.NS.String()+"/31", "dev", NSVeth); err != nil {
		return Pair{}, err
	}
	if _, err := s.IP(sys.Main, "link", "set", veth, "up"); err != nil {
		return Pair{}, err
	}
	if _, err := s.IP(ns, "link", "set", NSVeth, "up"); err != nil {
		return Pair{}, err
	}
	// The kernel installs the connected route when the address is first
	// added, but `ip addr replace` on an already-configured interface is a
	// no-op and does not bring a deleted route back; self-heal it, since
	// every mount route's `via` is validated against it.
	if out, err := s.IPQ(sys.Main, "route", "show", pair.Main.String()+"/31", "dev", veth); err == nil && strings.TrimSpace(out) == "" {
		if _, err := s.IP(sys.Main, "route", "add", pair.Main.String()+"/31", "dev", veth); err != nil {
			return Pair{}, err
		}
	}
	return pair, nil
}

// NATScripts returns the nft scripts for both sides of a mount: main
// masquerades into the veth address on its way into ve-<name>, the namespace
// masquerades the whole veth pool into whatever its egress is. Both are
// idempotent (flush + re-add of a per-connection chain).
func NATScripts(c *conn.Conn, pool netip.Prefix) (mainSide, nsSide string) {
	veth := c.Veth()
	mainSide = fmt.Sprintf(`table ip nsm {
	chain %[1]s {
		type nat hook postrouting priority srcnat; policy accept;
	}
}
flush chain ip nsm %[1]s
add rule ip nsm %[1]s oifname "%[1]s" masquerade
`, veth)
	nsSide = fmt.Sprintf(`table ip nsm {
	chain post {
		type nat hook postrouting priority srcnat; policy accept;
	}
}
flush chain ip nsm post
add rule ip nsm post ip saddr %s masquerade
`, pool)
	return
}

func ensureNAT(s *sys.Sys, c *conn.Conn, pool netip.Prefix) error {
	mainSide, nsSide := NATScripts(c, pool)
	if _, err := s.RunIn(mainSide, "nft", "-f", "-"); err != nil {
		return err
	}
	if _, err := s.InWithStdin(nsSide, c.NS(), "nft", "-f", "-"); err != nil {
		return err
	}
	_, err := s.In(c.NS(), "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")
	return err
}

// Mount converges main's view of c's exposed prefixes: creates the veth and
// NAT if needed, adds routes for mountable prefixes, refuses colliding ones,
// and drops routes left over from a previous expose= value. With an empty
// expose it tears the veth down.
func Mount(s *sys.Sys, c *conn.Conn, pool netip.Prefix) ([]Result, error) {
	if len(c.Meta.Expose) == 0 {
		Unmount(s, c)
		return nil, nil
	}
	pair, err := ensureVeth(s, c, pool)
	if err != nil {
		return nil, err
	}
	if err := ensureNAT(s, c, pool); err != nil {
		return nil, err
	}
	routes, err := mainRoutes(s)
	if err != nil {
		return nil, err
	}
	var results []Result
	wanted := map[netip.Prefix]bool{}
	for _, p := range c.Meta.Expose {
		wanted[p] = true
		res := Check(p, routes, c.Veth())
		if res.Mounted {
			if _, err := s.IP(sys.Main, "route", "replace", p.String(), "via", pair.NS.String(), "dev", c.Veth()); err != nil {
				res = Result{Prefix: p, Rejected: true, Reason: err.Error()}
			}
		}
		results = append(results, res)
	}
	for _, r := range routes {
		if r.Dev != c.Veth() {
			continue
		}
		// Never touch kernel connected routes (the veth's own /31) or
		// anything inside the veth pool: those are infrastructure, not
		// mounts — deleting the /31 had every later run's `via` rejected.
		if rp, ok := r.prefix(); ok && !wanted[rp] && r.Protocol != "kernel" && !pool.Contains(rp.Addr()) {
			s.TryIP(sys.Main, "route", "del", rp.String(), "dev", c.Veth())
		}
	}
	return results, nil
}

// Unmount removes the veth (both halves and the routes through it) and the
// main-side NAT chain. The namespace-side NAT dies with the namespace.
func Unmount(s *sys.Sys, c *conn.Conn) {
	if netns.LinkExists(s, sys.Main, c.Veth()) {
		s.TryIP(sys.Main, "link", "del", c.Veth())
	}
	s.Try("nft", "delete", "chain", "ip", "nsm", c.Veth())
}

// Status recomputes, from main's routing table alone, which exposed prefixes
// are mounted and why the others are not.
func Status(s *sys.Sys, c *conn.Conn) ([]Result, error) {
	routes, err := mainRoutes(s)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, p := range c.Meta.Expose {
		mounted := false
		for _, r := range routes {
			if rp, ok := r.prefix(); ok && r.Dev == c.Veth() && rp == p {
				mounted = true
			}
		}
		if mounted {
			results = append(results, Result{Prefix: p, Mounted: true})
			continue
		}
		res := Check(p, routes, c.Veth())
		if res.Mounted {
			res = Result{Prefix: p, Reason: "connection not run yet"}
		}
		results = append(results, res)
	}
	return results, nil
}
