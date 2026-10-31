// Package netns manages named network namespaces the iproute2 way
// (/run/netns/<name> bind mounts, /etc/netns/<name>/resolv.conf) and knows
// how to tell whether a namespace is still the one we started with.
package netns

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/akovalenko/nsm/internal/sys"
)

// EtcDir is where `ip netns exec` picks up per-namespace /etc overrides.
// A variable so tests can point it elsewhere.
var EtcDir = "/etc/netns"

// Path is the nsfs mount of a named namespace.
func Path(ns string) string { return filepath.Join(sys.NetnsDir, ns) }

// Exists reports whether the named namespace is mounted.
func Exists(ns string) bool {
	if ns == sys.Main {
		return true
	}
	_, err := os.Stat(Path(ns))
	return err == nil
}

// ID identifies a namespace instance: the nsfs inode. A namespace deleted
// and recreated under the same name gets a new ID, which is how a dependent
// notices its substrate was replaced underneath it. Main is identified by
// the caller's own namespace.
func ID(ns string) (uint64, error) {
	p := Path(ns)
	if ns == sys.Main {
		p = "/proc/self/ns/net"
	}
	ino, err := nsInode(p)
	if err != nil {
		return 0, fmt.Errorf("netns %s: %w", ns, err)
	}
	return ino, nil
}

func nsInode(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return st.Ino, nil
}

// Identify returns the name of the network namespace the process is in, or
// "" when it matches no named namespace (what `ip netns identify $$` does).
func Identify() string {
	ino, err := nsInode("/proc/self/ns/net")
	if err != nil {
		return ""
	}
	ents, err := os.ReadDir(sys.NetnsDir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if i, err := nsInode(filepath.Join(sys.NetnsDir, e.Name())); err == nil && i == ino {
			return e.Name()
		}
	}
	return ""
}

// MainCheck returns an error when the process sits in a network namespace
// other than init's (PID 1): nsm addresses "main" as its own namespace, so
// an accidental `ip netns exec X nsm run ...` would build all main-side
// plumbing inside X, silently. Indicators that cannot be read count as
// main — the check never blocks what it cannot see.
func MainCheck() error {
	self, err1 := nsInode("/proc/self/ns/net")
	init, err2 := nsInode("/proc/1/ns/net")
	if err1 != nil || err2 != nil || self == init {
		return nil
	}
	if who := Identify(); who != "" {
		return fmt.Errorf("current network namespace is %q, not the init (main) one", who)
	}
	return errors.New("current network namespace is not the init (main) one")
}

// Ensure creates the namespace if missing and brings its loopback up.
func Ensure(s *sys.Sys, ns string) error {
	if ns == sys.Main {
		return nil
	}
	if !Exists(ns) {
		if _, err := s.IP("", "netns", "add", ns); err != nil {
			return err
		}
	}
	_, err := s.IP(ns, "link", "set", "lo", "up")
	return err
}

// Delete removes the namespace and its /etc/netns overrides.
func Delete(s *sys.Sys, ns string) {
	if ns == sys.Main {
		return
	}
	if Exists(ns) {
		s.TryIP("", "netns", "del", ns)
	}
	if !s.DryRun {
		os.RemoveAll(filepath.Join(EtcDir, ns))
	}
}

// HasDefault reports whether ns has an IPv4 default route.
func HasDefault(s *sys.Sys, ns string) bool {
	out, err := s.IPQ(ns, "-j", "-4", "route", "show", "default")
	if err != nil {
		return false
	}
	var routes []json.RawMessage
	if json.Unmarshal([]byte(out), &routes) != nil {
		return false
	}
	return len(routes) > 0
}

// WriteResolv installs resolv.conf for ns (picked up by `ip netns exec` and
// `nsm exec`). Empty content removes the override.
func WriteResolv(s *sys.Sys, ns, content string) error {
	dir := filepath.Join(EtcDir, ns)
	path := filepath.Join(dir, "resolv.conf")
	if s.Verbose || s.DryRun {
		if content == "" {
			fmt.Fprintln(s.Log, "+ rm -f", path)
		} else {
			fmt.Fprintf(s.Log, "+ write %s (%d bytes)\n", path, len(content))
		}
	}
	if s.DryRun {
		return nil
	}
	if content == "" {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Substrate is what a connection stands on: its transport namespace, pinned
// to the instance seen at start.
type Substrate struct {
	NS string
	ID uint64
}

// WaitSubstrate blocks until ns exists and has a default route, or timeout
// passes. It is the start-side half of crash-only convergence: a dependent
// started before its substrate simply waits (and lets the supervisor restart
// it if the wait runs out).
func WaitSubstrate(s *sys.Sys, ns string, timeout time.Duration) (*Substrate, error) {
	deadline := time.Now().Add(timeout)
	for {
		if Exists(ns) && (HasDefault(s, ns) || s.DryRun) {
			id, err := ID(ns)
			if err == nil || s.DryRun {
				return &Substrate{NS: ns, ID: id}, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("substrate %s: no namespace with a default route after %s", ns, timeout)
		}
		time.Sleep(time.Second)
	}
}

// Check is the run-side half: it fails once the substrate namespace is gone,
// has been replaced by a new instance, or lost its default route.
func (sub *Substrate) Check(s *sys.Sys) error {
	if sub.NS == sys.Main {
		return nil
	}
	id, err := ID(sub.NS)
	if err != nil {
		return fmt.Errorf("substrate %s vanished", sub.NS)
	}
	if id != sub.ID {
		return fmt.Errorf("substrate %s was recreated", sub.NS)
	}
	if !HasDefault(s, sub.NS) {
		return fmt.Errorf("substrate %s lost its default route", sub.NS)
	}
	return nil
}

// Link is the subset of `ip -d -j link show` nsm reads.
type Link struct {
	Ifname   string `json:"ifname"`
	Flags    []string
	MTU      int `json:"mtu"`
	LinkInfo struct {
		Kind string `json:"info_kind"`
	} `json:"linkinfo"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		Prefixlen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// Up reports whether the link has the UP flag.
func (l Link) Up() bool {
	for _, f := range l.Flags {
		if f == "UP" {
			return true
		}
	}
	return false
}

// Links lists interfaces in ns (with addresses and kinds).
func Links(s *sys.Sys, ns string) ([]Link, error) {
	out, err := s.IPQ(ns, "-d", "-j", "addr", "show")
	if err != nil {
		return nil, err
	}
	var links []Link
	if out == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return nil, fmt.Errorf("ip -j addr: %w", err)
	}
	return links, nil
}

// LinkExists reports whether ifname is present in ns.
func LinkExists(s *sys.Sys, ns, ifname string) bool {
	_, err := s.IPQ(ns, "link", "show", "dev", ifname)
	return err == nil
}
