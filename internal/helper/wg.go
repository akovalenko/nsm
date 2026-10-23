package helper

import (
	"bufio"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akovalenko/nsm/internal/netns"
	"github.com/akovalenko/nsm/internal/sys"
)

// WGConf is a wg-quick style config split into the part `wg setconf`
// accepts and the wg-quick extras we apply ourselves.
type WGConf struct {
	Native    string // [Interface]/[Peer] text with wg-quick keys removed
	Addresses []netip.Prefix
	DNS       []string // nameservers from DNS=
	MTU       int
	Endpoints []string // host:port as written, before resolution
}

// wg-quick-only keys that wg(8) rejects; Table/hooks are meaningless here
// (routing inside the connection ns is always `default dev wg`).
var quickKeys = map[string]bool{
	"address": true, "dns": true, "mtu": true, "table": true, "saveconfig": true,
	"preup": true, "postup": true, "predown": true, "postdown": true,
}

// ParseWGConf splits a wg-quick config. Unknown keys are left for wg to
// judge, so native wg configs pass through untouched.
func ParseWGConf(text string) (WGConf, error) {
	var wc WGConf
	var out strings.Builder
	section := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.Trim(line, "[] "))
			out.WriteString(raw + "\n")
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			out.WriteString(raw + "\n")
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if section == "interface" && quickKeys[key] {
			switch key {
			case "address":
				for _, a := range splitList(val) {
					p, err := netip.ParsePrefix(a)
					if err != nil {
						addr, err2 := netip.ParseAddr(a)
						if err2 != nil {
							return wc, fmt.Errorf("Address %q: %v", a, err)
						}
						p = netip.PrefixFrom(addr, addr.BitLen())
					}
					wc.Addresses = append(wc.Addresses, p)
				}
			case "dns":
				wc.DNS = append(wc.DNS, splitList(val)...)
			case "mtu":
				n, err := strconv.Atoi(val)
				if err != nil {
					return wc, fmt.Errorf("MTU %q: %v", val, err)
				}
				wc.MTU = n
			}
			continue
		}
		if section == "peer" && key == "endpoint" {
			wc.Endpoints = append(wc.Endpoints, val)
		}
		out.WriteString(raw + "\n")
	}
	wc.Native = out.String()
	return wc, sc.Err()
}

func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
}

// ResolveEndpoints rewrites `Endpoint = host:port` lines with the host
// resolved by lookup. wg setconf would resolve them itself, but inside the
// connection ns, where there is no route out yet; the helper runs in the
// transport ns, so resolving here is both possible and the correct view.
func ResolveEndpoints(native string, lookup func(host string) (string, error)) (string, error) {
	var out strings.Builder
	for _, raw := range strings.SplitAfter(native, "\n") {
		line := strings.TrimSpace(raw)
		key, val, ok := strings.Cut(line, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "endpoint") {
			val = strings.TrimSpace(val)
			host, port, err := net.SplitHostPort(val)
			if err != nil {
				return "", fmt.Errorf("Endpoint %q: %v", val, err)
			}
			if _, err := netip.ParseAddr(host); err != nil {
				ip, err := lookup(host)
				if err != nil {
					return "", fmt.Errorf("Endpoint %s: %w", host, err)
				}
				host = ip
			}
			raw = fmt.Sprintf("Endpoint = %s\n", net.JoinHostPort(host, port))
		}
		out.WriteString(raw)
	}
	return out.String(), nil
}

// Getent resolves host to its first IPv4 address with getent(1) in the
// current namespace, i.e. with this namespace's resolv.conf.
func Getent(s *sys.Sys) func(host string) (string, error) {
	return func(host string) (string, error) {
		out, err := s.Query("getent", "ahostsv4", host)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", host, err)
		}
		f := strings.Fields(out)
		if len(f) == 0 {
			return "", fmt.Errorf("resolve %s: no address", host)
		}
		return f[0], nil
	}
}

// RunWG brings up a WireGuard interface for the connection and then holds
// it: the interface is created here (in the transport ns, which pins its
// UDP socket), moved into the connection ns, configured, and watched until
// we are told to stop or it disappears.
func RunWG(s *sys.Sys, e Env, confPath string) error {
	text, err := os.ReadFile(confPath)
	if err != nil {
		return err
	}
	wc, err := ParseWGConf(string(text))
	if err != nil {
		return fmt.Errorf("%s: %w", confPath, err)
	}
	native, err := ResolveEndpoints(wc.Native, Getent(s))
	if err != nil {
		return err
	}
	ifname := e.Name
	ns := e.NS

	// Converge from any stale state: a leftover interface in either ns.
	if netns.LinkExists(s, ns, ifname) {
		s.TryIP(ns, "link", "del", ifname)
	}
	if netns.LinkExists(s, "", ifname) {
		s.TryIP("", "link", "del", ifname)
	}
	if _, err := s.IP("", "link", "add", ifname, "type", "wireguard"); err != nil {
		return err
	}
	if _, err := s.IP("", "link", "set", ifname, "netns", ns); err != nil {
		return err
	}
	stateDir, err := e.StateDir()
	if err != nil {
		return err
	}
	derived := filepath.Join(stateDir, "wg.conf")
	if !s.DryRun {
		if err := os.WriteFile(derived, []byte(native), 0o600); err != nil {
			return err
		}
	}
	if _, err := s.In(ns, "wg", "setconf", ifname, derived); err != nil {
		return err
	}
	mtu := e.Meta.MTU
	if mtu == 0 {
		mtu = wc.MTU
	}
	if mtu != 0 {
		if _, err := s.IP(ns, "link", "set", ifname, "mtu", strconv.Itoa(mtu)); err != nil {
			return err
		}
	}
	for _, a := range wc.Addresses {
		if _, err := s.IP(ns, "addr", "replace", a.String(), "dev", ifname); err != nil {
			return err
		}
	}
	if _, err := s.IP(ns, "link", "set", ifname, "up"); err != nil {
		return err
	}
	if _, err := s.IP(ns, "-4", "route", "replace", "default", "dev", ifname); err != nil {
		return err
	}
	if e.Meta.Resolv == "" && len(wc.DNS) > 0 {
		var rc strings.Builder
		for _, d := range wc.DNS {
			if _, err := netip.ParseAddr(d); err == nil {
				fmt.Fprintf(&rc, "nameserver %s\n", d)
			} else {
				fmt.Fprintf(&rc, "search %s\n", d)
			}
		}
		if err := netns.WriteResolv(s, ns, rc.String()); err != nil {
			return err
		}
	}
	if s.DryRun {
		return nil
	}
	fmt.Fprintf(os.Stderr, "nsm helper wg: %s up in netns %s, holding\n", ifname, ns)
	err = hold(s, ns, ifname)
	s.TryIP(ns, "link", "del", ifname)
	return err
}

// hold blocks until a stop signal (nil) or until the interface is gone
// (error): the supervisor then restarts the whole run and converges again.
func hold(s *sys.Sys, ns, ifname string) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-sig:
			return nil
		case <-tick.C:
			if !netns.LinkExists(s, ns, ifname) {
				return fmt.Errorf("%s vanished from netns %s", ifname, ns)
			}
		}
	}
}
