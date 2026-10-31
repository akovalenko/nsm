package expose

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/akovalenko/nsm/internal/conn"
	"github.com/akovalenko/nsm/internal/sys"
)

var pool = netip.MustParsePrefix("169.254.100.0/23")

const mainRoutesJSON = `[
 {"dst":"default","gateway":"192.168.1.1","dev":"wlan0"},
 {"dst":"192.168.1.0/24","dev":"wlan0","protocol":"kernel","scope":"link"},
 {"dst":"169.254.100.2/31","dev":"ve-other","protocol":"kernel","scope":"link","prefsrc":"169.254.100.2"},
 {"dst":"10.10.0.0/24","dev":"ve-other","gateway":"169.254.100.3"},
 {"dst":"169.254.100.0/31","dev":"ve-client59","protocol":"kernel","scope":"link","prefsrc":"169.254.100.0"},
 {"dst":"10.99.0.0/16","dev":"ve-client59","gateway":"169.254.100.1"},
 {"dst":"172.16.5.7","dev":"tun9"}
]`

func TestCheck(t *testing.T) {
	var rs []route
	f := sys.NewFake()
	f.On("ip -j -4 route show table main", mainRoutesJSON)
	rs, err := mainRoutes(f.Sys)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		p      string
		ok     bool
		reason string
	}{
		{"10.48.0.0/16", true, ""},
		{"192.168.1.0/24", false, "overlaps 192.168.1.0/24 dev wlan0"},
		{"192.168.0.0/16", false, "overlaps 192.168.1.0/24 dev wlan0"},
		{"192.168.1.128/25", false, "overlaps 192.168.1.0/24 dev wlan0"},
		{"10.10.0.0/24", false, "overlaps 10.10.0.0/24 dev ve-other (mounted by other)"},
		{"10.99.0.0/16", true, ""}, // our own earlier mount
		{"10.99.5.0/24", true, ""}, // nested inside our own mount: fine
		{"172.16.0.0/12", false, "overlaps 172.16.5.7/32 dev tun9"},
		{"0.0.0.0/0", true, ""}, // default is not a collision partner; nothing else overlaps... except everything
	}
	for _, c := range cases {
		if c.p == "0.0.0.0/0" {
			// a /0 overlaps every route; make sure that is what we report
			r := Check(netip.MustParsePrefix(c.p), rs, "ve-client59")
			if r.Mounted {
				t.Errorf("/0 should be rejected, got mounted")
			}
			continue
		}
		r := Check(netip.MustParsePrefix(c.p), rs, "ve-client59")
		if r.Mounted != c.ok || r.Reason != c.reason {
			t.Errorf("%s: got %+v", c.p, r)
		}
	}
}

func TestPairAlloc(t *testing.T) {
	if p := pairAt(netip.MustParseAddr("169.254.100.7")); p.Main.String() != "169.254.100.6" || p.NS.String() != "169.254.100.7" {
		t.Errorf("pairAt = %+v", p)
	}
	f := sys.NewFake()
	f.On("ip -d -j addr show", `[
	 {"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},
	 {"ifname":"ve-other","addr_info":[{"family":"inet","local":"169.254.100.0","prefixlen":31}]},
	 {"ifname":"ve-third","addr_info":[{"family":"inet","local":"169.254.100.2","prefixlen":31}]}
	]`)
	c := &conn.Conn{Name: "client59"}
	p, err := allocPair(f.Sys, c, pool)
	if err != nil || p.Main.String() != "169.254.100.4" {
		t.Errorf("first free: %+v, %v", p, err)
	}
	f.On("ip -d -j addr show", `[{"ifname":"ve-client59","addr_info":[{"family":"inet","local":"169.254.100.8","prefixlen":31}]}]`)
	p, err = allocPair(f.Sys, c, pool)
	if err != nil || p.Main.String() != "169.254.100.8" {
		t.Errorf("reuse own: %+v, %v", p, err)
	}
}

func TestMountSequence(t *testing.T) {
	f := sys.NewFake()
	f.Fail("ip link show dev ve-client59", "does not exist") // veth missing: create it
	f.On("ip -d -j addr show", `[]`)
	f.On("ip -j -4 route show table main", mainRoutesJSON)
	c := &conn.Conn{Name: "client59", Meta: conn.Meta{Rides: "main",
		Expose: []netip.Prefix{netip.MustParsePrefix("10.48.0.0/16"), netip.MustParsePrefix("192.168.1.0/24")}}}
	res, err := Mount(f.Sys, c, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || !res[0].Mounted || res[1].Mounted || !strings.Contains(res[1].Reason, "wlan0") {
		t.Errorf("results = %v", res)
	}
	want := []string{
		"ip -n client59 link del ve0",
		"ip link add ve-client59 type veth peer name ve0 netns client59",
		"ip addr replace 169.254.100.0/31 dev ve-client59",
		"ip -n client59 addr replace 169.254.100.1/31 dev ve0",
		"ip link set ve-client59 up",
		"ip -n client59 link set ve0 up",
		"ip route add 169.254.100.0/31 dev ve-client59", // connected route self-heal
		"nft -f - <<< table ip nsm",
		"ip netns exec client59 nft -f - <<< table ip nsm",
		"ip netns exec client59 sysctl -q -w net.ipv4.ip_forward=1",
		"ip route replace 10.48.0.0/16 via 169.254.100.1 dev ve-client59",
		"ip route del 10.99.0.0/16 dev ve-client59", // stale mount from an older expose=
	}
	for _, w := range want {
		if !f.Called(w) {
			t.Errorf("missing call %q\nhave:\n%s", w, strings.Join(f.Calls, "\n"))
		}
	}
	if f.Called("ip route replace 192.168.1.0/24") {
		t.Error("rejected prefix must not be routed")
	}
	if f.Called("ip route del 169.254.100.0/31") || f.Called("ip route del 169.254.100.2/31") {
		t.Error("kernel connected routes of the veth pool must never be deleted")
	}
	mainSide, nsSide := NATScripts(c, pool)
	if !strings.Contains(mainSide, `oifname "ve-client59" masquerade`) || !strings.Contains(nsSide, "ip saddr 169.254.100.0/23 masquerade") {
		t.Errorf("nat scripts:\n%s\n%s", mainSide, nsSide)
	}
}

func TestMountEmptyExposeUnmounts(t *testing.T) {
	f := sys.NewFake()
	c := &conn.Conn{Name: "lappy"}
	if _, err := Mount(f.Sys, c, pool); err != nil {
		t.Fatal(err)
	}
	if !f.Called("ip link del ve-lappy") || !f.Called("nft delete chain ip nsm ve-lappy") {
		t.Errorf("calls: %v", f.Calls)
	}
}

func TestStatus(t *testing.T) {
	f := sys.NewFake()
	f.On("ip -j -4 route show table main", mainRoutesJSON)
	c := &conn.Conn{Name: "client59", Meta: conn.Meta{Expose: []netip.Prefix{
		netip.MustParsePrefix("10.99.0.0/16"), netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("10.48.0.0/16")}}}
	res, err := Status(f.Sys, c)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Mounted || res[1].Mounted || !strings.Contains(res[1].Reason, "wlan0") || res[2].Mounted || !!res[2].Rejected && strings.Contains(res[2].Reason, "not run") {
		t.Errorf("status = %v", res)
	}
}
