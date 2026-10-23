package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/akovalenko/nsm/internal/conn"
	"github.com/akovalenko/nsm/internal/expose"
	"github.com/akovalenko/nsm/internal/helper"
	"github.com/akovalenko/nsm/internal/netns"
	"github.com/akovalenko/nsm/internal/sys"
)

// cmdStatus has no state of its own: everything is recomputed from the
// connection directories, /run/netns, the links and routes in each ns and
// the pidfiles.
func (g *globals) cmdStatus(names []string) int {
	s := quiet(g.s)
	rc := 0
	all := len(names) == 0
	if all {
		var err error
		names, err = conn.List(g.confdir)
		if err != nil {
			return g.fail("%v", err)
		}
		if len(names) == 0 {
			fmt.Fprintf(g.out, "no connections in %s\n", g.confdir)
		}
	}
	known := map[string]bool{}
	for _, name := range names {
		known[name] = true
		c, err := conn.Load(g.confdir, name)
		if err != nil {
			fmt.Fprintf(g.out, "%s: %v\n", name, err)
			rc = 1
			continue
		}
		g.printStatus(s, c)
	}
	if all {
		g.printOrphans(s, known)
	}
	return rc
}

func (g *globals) printStatus(s *sys.Sys, c *conn.Conn) {
	w := g.out
	fmt.Fprintf(w, "%s  rides=%s\n", c.Name, c.Meta.Rides)
	stateDir := filepath.Join(helper.RunDir, c.Name)
	nannyPid := readPid(filepath.Join(stateDir, "nanny.pid"))
	runPid := readPid(filepath.Join(stateDir, "run.pid"))
	switch {
	case runPid != 0 && nannyPid != 0:
		fmt.Fprintf(w, "  transport: running (run pid %d, nanny pid %d)\n", runPid, nannyPid)
	case runPid != 0:
		fmt.Fprintf(w, "  transport: running (run pid %d, external supervisor)\n", runPid)
	case nannyPid != 0:
		fmt.Fprintf(w, "  transport: DOWN, nanny pid %d waiting to respawn\n", nannyPid)
	default:
		fmt.Fprintf(w, "  transport: DOWN\n")
	}
	if !netns.Exists(c.NS()) {
		fmt.Fprintf(w, "  netns: missing\n")
	} else {
		links, err := netns.Links(s, c.NS())
		if err != nil {
			fmt.Fprintf(w, "  netns: %v\n", err)
		}
		payload := 0
		for _, l := range links {
			if l.Ifname == "lo" || l.Ifname == expose.NSVeth {
				continue
			}
			payload++
			var addrs []string
			for _, a := range l.AddrInfo {
				if a.Family == "inet" {
					addrs = append(addrs, fmt.Sprintf("%s/%d", a.Local, a.Prefixlen))
				}
			}
			state := "DOWN"
			if l.Up() {
				state = "up"
			}
			kind := l.LinkInfo.Kind
			if kind == "" {
				kind = "?"
			}
			fmt.Fprintf(w, "  iface: %s (%s) %s %s mtu %d", l.Ifname, kind, strings.Join(addrs, ","), state, l.MTU)
			if kind == "wireguard" {
				fmt.Fprintf(w, ", handshake %s", wgHandshake(s, c.NS(), l.Ifname))
			}
			fmt.Fprintln(w)
		}
		if payload == 0 {
			fmt.Fprintf(w, "  iface: none (transport not up)\n")
		}
		if netns.HasDefault(s, c.NS()) {
			fmt.Fprintf(w, "  default route: yes\n")
		} else {
			fmt.Fprintf(w, "  default route: NO\n")
		}
	}
	if len(c.Meta.Expose) > 0 {
		res, err := expose.Status(s, c)
		if err != nil {
			fmt.Fprintf(w, "  mounts: %v\n", err)
		}
		for _, r := range res {
			fmt.Fprintf(w, "  mount: %s\n", r)
		}
	}
}

// wgHandshake summarises the freshest peer handshake of a wg interface.
func wgHandshake(s *sys.Sys, ns, ifname string) string {
	out, err := s.InQ(ns, "wg", "show", ifname, "latest-handshakes")
	if err != nil {
		return "?"
	}
	best := int64(0)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if t, err := strconv.ParseInt(f[1], 10, 64); err == nil && t > best {
			best = t
		}
	}
	if best == 0 {
		return "never"
	}
	return time.Since(time.Unix(best, 0)).Round(time.Second).String() + " ago"
}

// printOrphans lists kernel state that no connection directory explains.
func (g *globals) printOrphans(s *sys.Sys, known map[string]bool) {
	ents, _ := os.ReadDir(sys.NetnsDir)
	for _, e := range ents {
		if conn.ValidName(e.Name()) && !known[e.Name()] && looksOurs(s, e.Name()) {
			fmt.Fprintf(g.out, "orphan: netns %s has no connection directory (nsm down %s removes it)\n", e.Name(), e.Name())
		}
	}
	links, err := netns.Links(s, sys.Main)
	if err != nil {
		return
	}
	for _, l := range links {
		if name, ok := strings.CutPrefix(l.Ifname, "ve-"); ok && !known[name] {
			fmt.Fprintf(g.out, "orphan: veth %s in main has no connection directory\n", l.Ifname)
		}
	}
}

// looksOurs guesses whether a named netns was made by nsm: it has our veth
// half or nsm's nat table. Other tools' namespaces are not reported.
func looksOurs(s *sys.Sys, ns string) bool {
	if netns.LinkExists(s, ns, expose.NSVeth) {
		return true
	}
	_, err := s.InQ(ns, "nft", "list", "table", "ip", "nsm")
	return err == nil
}
