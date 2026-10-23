package helper

import (
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/akovalenko/nsm/internal/netns"
	"github.com/akovalenko/nsm/internal/sys"
)

// OpenVPNArgs builds the openvpn command line: the user's config as is,
// plus the flags that implement the fd trick. openvpn keeps the tun fd and
// its transport socket here in the transport ns; our --up handler moves the
// device into the connection ns and configures it from the environment
// openvpn hands to scripts (--ifconfig-noexec/--route-noexec make openvpn
// leave that to us).
func OpenVPNArgs(self, ovpn string, extra []string) []string {
	args := []string{
		"--cd", filepath.Dir(ovpn),
		"--config", filepath.Base(ovpn),
		"--ifconfig-noexec",
		"--route-noexec",
		"--script-security", "2",
		"--up", self + " helper openvpn-up",
		"--up-restart",
	}
	return append(args, extra...)
}

// RunOpenVPN runs openvpn in the foreground, forwarding stop signals.
func RunOpenVPN(s *sys.Sys, e Env, ovpn string, extra []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.ContainsAny(self, " \t") {
		return fmt.Errorf("nsm binary path %q contains whitespace; openvpn --up cannot quote it", self)
	}
	ovpn, err = filepath.Abs(ovpn)
	if err != nil {
		return err
	}
	cmd := s.Command("", "openvpn", OpenVPNArgs(self, ovpn, extra)...)
	if err := cmd.Start(); err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case sg := <-sig:
			_ = cmd.Process.Signal(sg)
		case err := <-done:
			return err
		}
	}
}

// OpenVPNUp is the --up handler. It is idempotent: on --up-restart with
// --persist-tun the device is already in place and only addressing/routes
// are reapplied.
func OpenVPNUp(s *sys.Sys, e Env, getenv func(string) string) error {
	dev := getenv("dev")
	if dev == "" {
		return fmt.Errorf("openvpn-up: $dev not set (not called by openvpn?)")
	}
	ns := e.NS
	if !netns.LinkExists(s, ns, dev) {
		if _, err := s.IP("", "link", "set", dev, "netns", ns); err != nil {
			return err
		}
	}
	local, remote, mask := getenv("ifconfig_local"), getenv("ifconfig_remote"), getenv("ifconfig_netmask")
	switch {
	case local == "":
		return fmt.Errorf("openvpn-up: $ifconfig_local not set")
	case mask != "":
		bits, err := maskBits(mask)
		if err != nil {
			return err
		}
		if _, err := s.IP(ns, "addr", "replace", fmt.Sprintf("%s/%d", local, bits), "dev", dev); err != nil {
			return err
		}
	case remote != "":
		if _, err := s.IP(ns, "addr", "replace", local, "peer", remote+"/32", "dev", dev); err != nil {
			return err
		}
	default:
		if _, err := s.IP(ns, "addr", "replace", local+"/32", "dev", dev); err != nil {
			return err
		}
	}
	mtu := e.Meta.MTU
	if mtu == 0 {
		mtu, _ = strconv.Atoi(getenv("tun_mtu"))
	}
	if mtu != 0 {
		if _, err := s.IP(ns, "link", "set", dev, "mtu", strconv.Itoa(mtu)); err != nil {
			return err
		}
	}
	if _, err := s.IP(ns, "link", "set", dev, "up"); err != nil {
		return err
	}
	gw := getenv("route_vpn_gateway")
	if err := addRoute(s, ns, dev, "default", gw); err != nil {
		return err
	}
	for i := 1; ; i++ {
		network := getenv("route_network_" + strconv.Itoa(i))
		if network == "" {
			break
		}
		bits, err := maskBits(getenv("route_netmask_" + strconv.Itoa(i)))
		if err != nil {
			return err
		}
		rgw := getenv("route_gateway_" + strconv.Itoa(i))
		if rgw == "" {
			rgw = gw
		}
		if err := addRoute(s, ns, dev, fmt.Sprintf("%s/%d", network, bits), rgw); err != nil {
			return err
		}
	}
	if e.Meta.Resolv == "" {
		if rc := pushedResolv(getenv); rc != "" {
			if err := netns.WriteResolv(s, ns, rc); err != nil {
				return err
			}
		}
	}
	return nil
}

func addRoute(s *sys.Sys, ns, dev, dst, gw string) error {
	args := []string{"-4", "route", "replace", dst}
	if gw != "" {
		args = append(args, "via", gw, "dev", dev, "onlink")
	} else {
		args = append(args, "dev", dev)
	}
	_, err := s.IP(ns, args...)
	return err
}

func maskBits(mask string) (int, error) {
	if mask == "" {
		return 32, nil
	}
	a, err := netip.ParseAddr(mask)
	if err != nil || !a.Is4() {
		return 0, fmt.Errorf("netmask %q: not an IPv4 mask", mask)
	}
	b := a.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	bits := 0
	for v&0x80000000 != 0 {
		bits++
		v <<= 1
	}
	if v != 0 {
		return 0, fmt.Errorf("netmask %q: not contiguous", mask)
	}
	return bits, nil
}

// pushedResolv turns `dhcp-option DNS/DOMAIN` pushes into resolv.conf text.
func pushedResolv(getenv func(string) string) string {
	var ns, search []string
	for i := 1; ; i++ {
		opt := getenv("foreign_option_" + strconv.Itoa(i))
		if opt == "" {
			break
		}
		f := strings.Fields(opt)
		if len(f) != 3 || f[0] != "dhcp-option" {
			continue
		}
		switch strings.ToUpper(f[1]) {
		case "DNS":
			ns = append(ns, f[2])
		case "DOMAIN", "DOMAIN-SEARCH":
			search = append(search, f[2])
		}
	}
	if len(ns) == 0 {
		return ""
	}
	var b strings.Builder
	for _, n := range ns {
		fmt.Fprintf(&b, "nameserver %s\n", n)
	}
	if len(search) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(search, " "))
	}
	return b.String()
}
