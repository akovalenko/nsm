package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akovalenko/nsm/internal/conn"
	"github.com/akovalenko/nsm/internal/netns"
	"github.com/akovalenko/nsm/internal/sys"
)

const quickConf = `[Interface]
PrivateKey = cGxhY2Vob2xkZXJwcml2YXRla2V5cGxhY2Vob2xkZXI=
Address = 10.7.0.2/32, fd42::2/128
DNS = 10.7.0.1, corp.lan
MTU = 1370
Table = off
PostUp = echo hi   # wg-quick hook, dropped
ListenPort = 51820

[Peer]
PublicKey = cGxhY2Vob2xkZXJwdWJsaWNrZXlwbGFjZWhvbGRlcnB1Yg=
AllowedIPs = 0.0.0.0/0
Endpoint = vpn.example.org:51820
PersistentKeepalive = 25
`

func TestParseWGConf(t *testing.T) {
	wc, err := ParseWGConf(quickConf)
	if err != nil {
		t.Fatal(err)
	}
	if len(wc.Addresses) != 2 || wc.Addresses[0].String() != "10.7.0.2/32" {
		t.Errorf("addresses = %v", wc.Addresses)
	}
	if strings.Join(wc.DNS, ",") != "10.7.0.1,corp.lan" || wc.MTU != 1370 {
		t.Errorf("dns=%v mtu=%d", wc.DNS, wc.MTU)
	}
	if len(wc.Endpoints) != 1 || wc.Endpoints[0] != "vpn.example.org:51820" {
		t.Errorf("endpoints = %v", wc.Endpoints)
	}
	for _, gone := range []string{"Address", "DNS", "MTU", "Table", "PostUp"} {
		if strings.Contains(wc.Native, gone) {
			t.Errorf("native still has %s:\n%s", gone, wc.Native)
		}
	}
	for _, kept := range []string{"PrivateKey", "ListenPort", "[Peer]", "AllowedIPs", "PersistentKeepalive"} {
		if !strings.Contains(wc.Native, kept) {
			t.Errorf("native lost %s:\n%s", kept, wc.Native)
		}
	}
}

func TestResolveEndpoints(t *testing.T) {
	lookup := func(h string) (string, error) {
		if h == "vpn.example.org" {
			return "203.0.113.5", nil
		}
		t.Errorf("unexpected lookup %q", h)
		return "", nil
	}
	in := "Endpoint = vpn.example.org:51820\nEndpoint = 198.51.100.1:1\nEndpoint = [2001:db8::1]:2\nAllowedIPs = 0.0.0.0/0\n"
	out, err := ResolveEndpoints(in, lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := "Endpoint = 203.0.113.5:51820\nEndpoint = 198.51.100.1:1\nEndpoint = [2001:db8::1]:2\nAllowedIPs = 0.0.0.0/0\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestGetent(t *testing.T) {
	f := sys.NewFake()
	f.On("getent ahostsv4 vpn.example.org", "203.0.113.5    STREAM vpn.example.org\n203.0.113.5    DGRAM  \n")
	ip, err := Getent(f.Sys)("vpn.example.org")
	if err != nil || ip != "203.0.113.5" {
		t.Errorf("got %q, %v", ip, err)
	}
	f.Fail("getent ahostsv4 nowhere", "")
	if _, err := Getent(f.Sys)("nowhere.invalid"); err == nil {
		t.Error("expected failure")
	}
}

func TestOpenVPNArgs(t *testing.T) {
	args := OpenVPNArgs("/usr/local/bin/nsm", "/etc/nsm/client59/conf/client59.ovpn", []string{"--verb", "1"})
	got := strings.Join(args, " ")
	for _, w := range []string{"--cd /etc/nsm/client59/conf", "--config client59.ovpn", "--ifconfig-noexec", "--route-noexec",
		"--script-security 2", "--up /usr/local/bin/nsm helper openvpn-up", "--up-restart", "--verb 1"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestOpenVPNUpSubnetTopology(t *testing.T) {
	netns.EtcDir = t.TempDir()
	f := sys.NewFake()
	f.Fail("ip -n client59 link show dev tun0", "no such device") // not yet moved
	e := Env{Name: "client59", NS: "client59", Rides: "main"}
	err := OpenVPNUp(f.Sys, e, env(map[string]string{
		"dev": "tun0", "tun_mtu": "1500",
		"ifconfig_local": "10.8.0.6", "ifconfig_netmask": "255.255.255.0",
		"route_vpn_gateway": "10.8.0.1",
		"route_network_1":   "10.48.0.0", "route_netmask_1": "255.255.0.0", "route_gateway_1": "10.8.0.1",
		"route_network_2": "192.168.1.0", "route_netmask_2": "255.255.255.0",
		"foreign_option_1": "dhcp-option DNS 10.48.1.53",
		"foreign_option_2": "dhcp-option DOMAIN eputs.spd",
		"foreign_option_3": "dhcp-option DNS 10.48.1.54",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ip link set tun0 netns client59",
		"ip -n client59 addr replace 10.8.0.6/24 dev tun0",
		"ip -n client59 link set tun0 mtu 1500",
		"ip -n client59 link set tun0 up",
		"ip -n client59 -4 route replace default via 10.8.0.1 dev tun0 onlink",
		"ip -n client59 -4 route replace 10.48.0.0/16 via 10.8.0.1 dev tun0 onlink",
		"ip -n client59 -4 route replace 192.168.1.0/24 via 10.8.0.1 dev tun0 onlink",
	}
	for i, w := range want {
		if i+1 >= len(f.Calls) || f.Calls[i+1] != w {
			t.Fatalf("call %d = %q, want %q\nall:\n%s", i+1, f.Calls[i+1], w, strings.Join(f.Calls, "\n"))
		}
	}
	rc := pushedResolv(env(map[string]string{
		"foreign_option_1": "dhcp-option DNS 10.48.1.53", "foreign_option_2": "dhcp-option DOMAIN eputs.spd",
		"foreign_option_3": "dhcp-option DNS 10.48.1.54"}))
	if rc != "nameserver 10.48.1.53\nnameserver 10.48.1.54\nsearch eputs.spd\n" {
		t.Errorf("resolv = %q", rc)
	}
	b, err := os.ReadFile(filepath.Join(netns.EtcDir, "client59", "resolv.conf"))
	if err != nil || string(b) != rc {
		t.Errorf("written resolv.conf = %q, %v", b, err)
	}
}

func TestOpenVPNUpNet30AndRestart(t *testing.T) {
	f := sys.NewFake()
	// Device already in the connection ns (restart with persist-tun): no move.
	e := Env{Name: "c", NS: "c", Meta: conn.Meta{MTU: 1300, Resolv: "nameserver 1.1.1.1"}}
	err := OpenVPNUp(f.Sys, e, env(map[string]string{
		"dev": "tun3", "tun_mtu": "1500",
		"ifconfig_local": "10.9.0.6", "ifconfig_remote": "10.9.0.5",
		"foreign_option_1": "dhcp-option DNS 10.9.0.1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if f.Called("ip link set tun3 netns") {
		t.Error("device must not be moved twice")
	}
	for _, w := range []string{
		"ip -n c addr replace 10.9.0.6 peer 10.9.0.5/32 dev tun3",
		"ip -n c link set tun3 mtu 1300", // meta wins over tun_mtu
		"ip -n c -4 route replace default dev tun3",
	} {
		if !f.Called(w) {
			t.Errorf("missing %q in\n%s", w, strings.Join(f.Calls, "\n"))
		}
	}
	if f.Called("write") {
		t.Error("meta resolv= set: pushed DNS must be ignored")
	}
}

func TestMaskBits(t *testing.T) {
	for m, want := range map[string]int{"255.255.255.0": 24, "255.255.0.0": 16, "255.255.255.255": 32, "128.0.0.0": 1, "0.0.0.0": 0, "": 32} {
		if got, err := maskBits(m); err != nil || got != want {
			t.Errorf("%q: %d, %v", m, got, err)
		}
	}
	if _, err := maskBits("255.0.255.0"); err == nil {
		t.Error("non-contiguous mask accepted")
	}
}
