// Package expose implements the veth between main and a connection's
// namespace and what rides on it: the mount protocol (explicit routes in
// main for the exposed prefixes, double NAT, loud refusal when a prefix
// collides with what main already has), the uplink of a transport-less
// connection (the same veth the other way round: the namespace's default
// route and NAT on main's side), and the shorthand policy rules for traffic
// that arrives from the veth.
package expose

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
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
// expose it tears the veth down -- unless the veth is the connection's
// uplink, which is Uplink's to build.
func Mount(s *sys.Sys, c *conn.Conn, pool netip.Prefix) ([]Result, error) {
	if len(c.Meta.Expose) == 0 {
		if !c.Meta.Uplink {
			Unmount(s, c)
		}
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
	dropStaleRoutes(s, routes, c.Veth(), wanted, pool)
	return results, nil
}

// dropStaleRoutes removes the routes through veth that are neither mounts
// wanted now nor infrastructure. Never touched: kernel connected routes (the
// veth's own /31) and anything inside the veth pool -- deleting the /31 had
// every later run's `via` rejected.
func dropStaleRoutes(s *sys.Sys, routes []route, veth string, wanted map[netip.Prefix]bool, pool netip.Prefix) {
	for _, r := range routes {
		if r.Dev != veth {
			continue
		}
		if rp, ok := r.prefix(); ok && !wanted[rp] && r.Protocol != "kernel" && !pool.Contains(rp.Addr()) {
			s.TryIP(sys.Main, "route", "del", rp.String(), "dev", veth)
		}
	}
}

// Uplink converges the uplink of a transport-less connection (meta
// `uplink=`): the veth pair -- kept, with its address, from whichever run
// built it before --, the shorthand rules when meta asks for them, NAT on
// main's side, forwarding, and last the namespace's default route towards
// main. Last on purpose: a rule whose table holds no route is not final, so
// until the rules that keep this namespace out of main's tunnels stand, a
// default route here is one fall-through away from wherever main's own
// rules send unmarked traffic.
func Uplink(s *sys.Sys, c *conn.Conn, pool netip.Prefix) error {
	pair, err := ensureVeth(s, c, pool)
	if err != nil {
		return err
	}
	if routes, err := mainRoutes(s); err == nil {
		// a previous expose= on this connection left its mounts behind
		dropStaleRoutes(s, routes, c.Veth(), nil, pool)
	}
	if err := EnsureRule(s, c); err != nil {
		return err
	}
	if _, err := s.RunIn(uplinkNAT(c.Veth(), pair.NS), "nft", "-f", "-"); err != nil {
		return err
	}
	// Main forwards between the veth and its own uplink. Mounts never
	// needed that on this side (the namespace did the forwarding), so it
	// cannot be counted on.
	if _, err := s.Run("sysctl", "-q", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}
	_, err = s.IP(c.NS(), "-4", "route", "replace", "default", "via", pair.Main.String(), "dev", NSVeth)
	return err
}

// uplinkNAT is the main-side NAT of an uplink: the namespace's only address
// is a link-local one out of the veth pool, so on its way into main it is
// masqueraded into the uplink's address. Same chain and table as a mount's
// main side, so Unmount takes this one too. Idempotent.
func uplinkNAT(veth string, ns netip.Addr) string {
	return fmt.Sprintf(`table ip nsm {
	chain %[1]s {
		type nat hook postrouting priority srcnat; policy accept;
	}
}
flush chain ip nsm %[1]s
add rule ip nsm %[1]s ip saddr %[2]s oifname != "%[1]s" masquerade
`, veth, ns)
}

// rule is the subset of `ip -j -4 rule` nsm reads.
type rule struct {
	Priority int    `json:"priority"`
	Iif      string `json:"iif"`
	Table    string `json:"table"`
	Action   string `json:"action"`
}

func mainRules(s *sys.Sys) ([]rule, error) {
	out, err := s.IPQ(sys.Main, "-j", "-4", "rule", "show")
	if err != nil {
		return nil, err
	}
	var rs []rule
	if out == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(out), &rs); err != nil {
		return nil, fmt.Errorf("ip -j rule: %w", err)
	}
	return rs, nil
}

// canonTable maps what `ip rule` prints for the three reserved tables onto
// their numbers, so "254" and "main" compare equal.
func canonTable(t string) string {
	switch t {
	case "local":
		return "255"
	case sys.Main:
		return "254"
	case "default":
		return "253"
	}
	return t
}

// classify splits the rules that name c's veth into the shorthand's two and
// the rest: whatever else names the veth is a leftover of an older rule=
// value, or someone's own rule -- with rule= in force nsm owns the name.
func classify(c *conn.Conn, rs []rule) (lookup, unreachable bool, stale []rule) {
	w := c.Meta.Rule
	for _, r := range rs {
		if r.Iif != c.Veth() {
			continue
		}
		switch {
		case r.Priority == w.Pref+1 && r.Action == "unreachable":
			unreachable = true
		case r.Priority == w.Pref && r.Table != "" && canonTable(r.Table) == canonTable(w.Table):
			lookup = true
		default:
			stale = append(stale, r)
		}
	}
	return lookup, unreachable, stale
}

// EnsureRule converges the shorthand pair (meta `rule=`) from main's rules.
// A rule that is already right stays where it is -- no delete-and-add
// window on a restart. A missing one is added, the unreachable companion
// first: between the two adds the namespace is cut off rather than routed
// by whatever rules follow. Only then do the leftovers go, so a changed
// priority is a hand-over, not a gap.
func EnsureRule(s *sys.Sys, c *conn.Conn) error {
	w := c.Meta.Rule
	if w == nil {
		return nil
	}
	have, err := mainRules(s)
	if err != nil {
		return err
	}
	lookup, unreachable, stale := classify(c, have)
	if !unreachable {
		if _, err := s.IP(sys.Main, "-4", "rule", "add", "pref", strconv.Itoa(w.Pref+1), "iif", c.Veth(), "unreachable"); err != nil {
			return err
		}
	}
	if !lookup {
		if _, err := s.IP(sys.Main, "-4", "rule", "add", "pref", strconv.Itoa(w.Pref), "iif", c.Veth(), "lookup", w.Table); err != nil {
			return err
		}
	}
	return dropRules(s, c, stale)
}

// dropRules removes rules that name the connection's veth. While the veth
// exists the selector names it, and a foreign rule at the same priority is
// left alone; a detached one -- its device gone, the kernel keeps it and
// still shows the name -- can only be matched by priority and action.
func dropRules(s *sys.Sys, c *conn.Conn, rs []rule) error {
	if len(rs) == 0 {
		return nil
	}
	withVeth := netns.LinkExists(s, sys.Main, c.Veth())
	for _, r := range rs {
		args := []string{"-4", "rule", "del", "pref", strconv.Itoa(r.Priority)}
		if withVeth {
			args = append(args, "iif", c.Veth())
		}
		if r.Action != "" {
			args = append(args, r.Action)
		} else {
			args = append(args, "lookup", r.Table)
		}
		if _, err := s.IP(sys.Main, args...); err != nil {
			return err
		}
	}
	return nil
}

// RemoveRule deletes every rule that names the connection's veth as its
// input, detached ones included. The name is all it needs, so it also works
// for rules of a previous rule= value and after the connection directory is
// gone.
func RemoveRule(s *sys.Sys, c *conn.Conn) error {
	have, err := mainRules(s)
	if err != nil {
		return err
	}
	var mine []rule
	for _, r := range have {
		if r.Iif == c.Veth() {
			mine = append(mine, r)
		}
	}
	return dropRules(s, c, mine)
}

// RuleState is one of the shorthand's rules as main has it, or has it not.
type RuleState struct {
	Pref    int
	What    string
	Present bool
}

func (r RuleState) String() string {
	if !r.Present {
		return fmt.Sprintf("%d %s MISSING", r.Pref, r.What)
	}
	return fmt.Sprintf("%d %s", r.Pref, r.What)
}

// RuleStatus recomputes, from main's rules, which of the shorthand's two
// are in place.
func RuleStatus(s *sys.Sys, c *conn.Conn) ([]RuleState, error) {
	w := c.Meta.Rule
	if w == nil {
		return nil, nil
	}
	have, err := mainRules(s)
	if err != nil {
		return nil, err
	}
	lookup, unreachable, _ := classify(c, have)
	return []RuleState{
		{w.Pref, "lookup " + w.Table, lookup},
		{w.Pref + 1, "unreachable", unreachable},
	}, nil
}

// UplinkState is what main and the namespace have for an uplink.
type UplinkState struct {
	Veth    string // "<veth> <addr>/31" as main has it; "" when the veth is missing
	NAT     bool   // the masquerading chain is in table ip nsm
	Default string // the namespace's default route through ve0; "" when there is none
}

// OK reports whether every part of the uplink is in place.
func (u UplinkState) OK() bool { return u.Veth != "" && u.NAT && u.Default != "" }

func (u UplinkState) String() string {
	var missing []string
	if u.Veth == "" {
		missing = append(missing, "veth missing in main")
	}
	if !u.NAT {
		missing = append(missing, "no NAT chain")
	}
	if u.Default == "" {
		missing = append(missing, "no default route through "+NSVeth)
	}
	if len(missing) > 0 {
		return strings.Join(missing, ", ")
	}
	return fmt.Sprintf("%s, nat, default %s", u.Veth, u.Default)
}

// UplinkStatus recomputes the uplink from main and the namespace.
func UplinkStatus(s *sys.Sys, c *conn.Conn) (UplinkState, error) {
	var st UplinkState
	links, err := netns.Links(s, sys.Main)
	if err != nil {
		return st, err
	}
	for _, l := range links {
		if l.Ifname != c.Veth() {
			continue
		}
		for _, a := range l.AddrInfo {
			if a.Family == "inet" {
				st.Veth = fmt.Sprintf("%s %s/%d", l.Ifname, a.Local, a.Prefixlen)
			}
		}
	}
	if out, err := s.Query("nft", "list", "chain", "ip", "nsm", c.Veth()); err == nil && strings.Contains(out, "masquerade") {
		st.NAT = true
	}
	out, err := s.IPQ(c.NS(), "-j", "-4", "route", "show", "default")
	if err != nil {
		return st, nil // no namespace yet: nothing to ask
	}
	var rs []struct {
		Gateway string `json:"gateway"`
		Dev     string `json:"dev"`
	}
	if json.Unmarshal([]byte(out), &rs) == nil {
		for _, r := range rs {
			if r.Dev == NSVeth {
				st.Default = "via " + r.Gateway + " dev " + NSVeth
			}
		}
	}
	return st, nil
}

// Unmount removes the rules that name the veth, the veth itself (both
// halves and the routes through it) and the main-side NAT chain. The
// namespace-side NAT of a mount dies with the namespace.
func Unmount(s *sys.Sys, c *conn.Conn) {
	// Rules first: while the veth exists they can be selected by it.
	if err := RemoveRule(s, c); err != nil && s.Verbose {
		fmt.Fprintln(s.Log, "  (ignored)", err)
	}
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
