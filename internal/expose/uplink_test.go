package expose

import (
	"strconv"
	"strings"
	"testing"

	"github.com/akovalenko/nsm/internal/conn"
	"github.com/akovalenko/nsm/internal/sys"
)

func uplinkConn(rule *conn.Rule) *conn.Conn {
	return &conn.Conn{Name: "bare", Meta: conn.Meta{Rides: sys.Main, Uplink: true, Rule: rule}}
}

func calls(f *sys.Fake, prefix string) []string {
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestUplinkSequence(t *testing.T) {
	f := sys.NewFake()
	f.Fail("ip link show dev ve-bare", "does not exist") // veth missing: create it
	f.On("ip -d -j addr show", `[]`)                     // pool untouched: first /31
	f.On("ip -j -4 route show table main", mainRoutesJSON)
	c := uplinkConn(&conn.Rule{Pref: 30200, Table: sys.Main})
	if err := Uplink(f.Sys, c, pool); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ip link add ve-bare type veth peer name ve0 netns bare",
		"ip addr replace 169.254.100.0/31 dev ve-bare",
		"ip -n bare addr replace 169.254.100.1/31 dev ve0",
		"ip -4 rule add pref 30201 iif ve-bare unreachable", // cut off first,
		"ip -4 rule add pref 30200 iif ve-bare lookup main", // then let through
		"nft -f -",
		"sysctl -q -w net.ipv4.ip_forward=1",
		"ip -n bare -4 route replace default via 169.254.100.0 dev ve0", // last
	}
	var got []string
	for _, w := range want {
		for _, c := range f.Calls {
			if strings.HasPrefix(c, w) {
				got = append(got, w)
			}
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("order:\n%s\nhave:\n%s", strings.Join(want, "\n"), strings.Join(f.Calls, "\n"))
	}
	nat := calls(f, "nft -f -")
	if len(nat) != 1 || !strings.Contains(nat[0], `ip saddr 169.254.100.1 oifname != "ve-bare" masquerade`) ||
		!strings.Contains(nat[0], "chain ve-bare") {
		t.Errorf("uplink nat:\n%s", strings.Join(nat, "\n"))
	}
}

func TestUplinkConverges(t *testing.T) {
	f := sys.NewFake() // veth exists, rules in place
	f.On("ip -d -j addr show", `[{"ifname":"ve-bare","addr_info":[{"family":"inet","local":"169.254.100.8","prefixlen":31}]}]`)
	f.On("ip -j -4 route show table main", mainRoutesJSON)
	f.On("ip -j -4 rule show", pairJSON(30200, "main"))
	c := uplinkConn(&conn.Rule{Pref: 30200, Table: sys.Main})
	if err := Uplink(f.Sys, c, pool); err != nil {
		t.Fatal(err)
	}
	if f.Called("ip -4 rule del") || f.Called("ip -4 rule add") {
		t.Errorf("rules in place must be left alone:\n%s", strings.Join(calls(f, "ip -4 rule"), "\n"))
	}
	if f.Called("ip link add") || f.Called("ip link del") {
		t.Error("an existing veth is kept")
	}
	if !f.Called("ip -n bare -4 route replace default via 169.254.100.8 dev ve0") {
		t.Error("the default route must follow the address the veth already has")
	}
}

// Without the shorthand nsm installs no rule at all: they are the user's to
// bring, by any means, on iif ve-<name>.
func TestUplinkWithoutRule(t *testing.T) {
	f := sys.NewFake()
	f.On("ip -j -4 rule show", `[]`)
	if err := Uplink(f.Sys, uplinkConn(nil), pool); err != nil {
		t.Fatal(err)
	}
	if len(calls(f, "ip -4 rule")) != 0 || f.Called("ip -j -4 rule show") {
		t.Errorf("no rule work without rule=:\n%s", strings.Join(f.Calls, "\n"))
	}
}

// pairJSON is what `ip -j -4 rule show` prints for the shorthand's pair.
func pairJSON(pref int, table string) string {
	return `[{"priority":` + strconv.Itoa(pref) + `,"src":"all","iif":"ve-bare","table":"` + table + `"},` +
		`{"priority":` + strconv.Itoa(pref+1) + `,"src":"all","iif":"ve-bare","action":"unreachable"}]`
}

func TestEnsureRule(t *testing.T) {
	c := uplinkConn(&conn.Rule{Pref: 30200, Table: sys.Main})
	t.Run("absent", func(t *testing.T) {
		f := sys.NewFake()
		f.On("ip -j -4 rule show", `[{"priority":30000,"src":"all","table":"main"}]`)
		if err := EnsureRule(f.Sys, c); err != nil {
			t.Fatal(err)
		}
		if f.Called("ip -4 rule del") || !f.Called("ip -4 rule add pref 30201 iif ve-bare unreachable") || !f.Called("ip -4 rule add pref 30200 iif ve-bare lookup main") {
			t.Errorf("calls:\n%s", strings.Join(f.Calls, "\n"))
		}
	})
	t.Run("present", func(t *testing.T) {
		f := sys.NewFake()
		f.On("ip -j -4 rule show", pairJSON(30200, "main"))
		if err := EnsureRule(f.Sys, c); err != nil {
			t.Fatal(err)
		}
		if len(f.Calls) != 1 {
			t.Errorf("nothing to do when the pair is in place:\n%s", strings.Join(f.Calls, "\n"))
		}
	})
	t.Run("leftovers of an older value", func(t *testing.T) {
		// a pair at another priority, a lookup at ours with another table,
		// someone's own rule on the name -- and a foreign rule at our
		// priority, which is none of our business.
		f := sys.NewFake()
		f.On("ip -j -4 rule show", `[
		 {"priority":30000,"src":"all","iif":"ve-bare","table":"main"},
		 {"priority":30001,"src":"all","iif":"ve-bare","action":"unreachable"},
		 {"priority":30500,"src":"all","iif":"ve-bare","dst":"10.0.0.0/8","table":"5"},
		 {"priority":30200,"src":"all","iif":"ve-bare","table":"5300"},
		 {"priority":30200,"src":"all","uidrange":"128-128","table":"main"},
		 {"priority":30201,"src":"all","iif":"ve-bare","action":"unreachable"}
		]`)
		if err := EnsureRule(f.Sys, c); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"ip -4 rule add pref 30200 iif ve-bare lookup main", // the new pair first,
			"ip -4 rule del pref 30000 iif ve-bare lookup main", // then the hand-over
			"ip -4 rule del pref 30001 iif ve-bare unreachable",
			"ip -4 rule del pref 30500 iif ve-bare lookup 5",
			"ip -4 rule del pref 30200 iif ve-bare lookup 5300",
		}
		if got := calls(f, "ip -4 rule"); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("calls:\n%s", strings.Join(f.Calls, "\n"))
		}
	})
	t.Run("detached", func(t *testing.T) {
		// the veth is gone: the kernel keeps its rules, but they cannot be
		// selected by the device any more
		f := sys.NewFake()
		f.Fail("ip link show dev ve-bare", "does not exist")
		f.On("ip -j -4 rule show", `[{"priority":30200,"iif_detached":null,"src":"all","iif":"ve-bare","table":"5300"}]`)
		if err := EnsureRule(f.Sys, c); err != nil {
			t.Fatal(err)
		}
		if !f.Called("ip -4 rule del pref 30200 lookup 5300") || f.Called("iif ve-bare lookup 5300") {
			t.Errorf("calls:\n%s", strings.Join(f.Calls, "\n"))
		}
	})
	t.Run("table names", func(t *testing.T) {
		// what ip prints for the reserved tables is their name; it must
		// compare equal to the number
		f := sys.NewFake()
		f.On("ip -j -4 rule show", pairJSON(30200, "main"))
		if err := EnsureRule(f.Sys, uplinkConn(&conn.Rule{Pref: 30200, Table: "254"})); err != nil {
			t.Fatal(err)
		}
		if len(f.Calls) != 1 {
			t.Errorf("254 is main:\n%s", strings.Join(f.Calls, "\n"))
		}
	})
}

func TestRemoveRule(t *testing.T) {
	t.Run("with the veth", func(t *testing.T) {
		f := sys.NewFake()
		f.On("ip -j -4 rule show", pairJSON(30200, "main")[:len(pairJSON(30200, "main"))-1]+`,{"priority":31006,"src":"all","table":"main"}]`)
		if err := RemoveRule(f.Sys, &conn.Conn{Name: "bare"}); err != nil { // no meta needed
			t.Fatal(err)
		}
		want := []string{"ip -4 rule del pref 30200 iif ve-bare lookup main", "ip -4 rule del pref 30201 iif ve-bare unreachable"}
		if got := calls(f, "ip -4 rule"); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("calls:\n%s", strings.Join(f.Calls, "\n"))
		}
	})
	t.Run("detached", func(t *testing.T) {
		f := sys.NewFake()
		f.Fail("ip link show dev ve-bare", "does not exist")
		f.On("ip -j -4 rule show", pairJSON(30200, "main"))
		if err := RemoveRule(f.Sys, &conn.Conn{Name: "bare"}); err != nil {
			t.Fatal(err)
		}
		if !f.Called("ip -4 rule del pref 30200 lookup main") || !f.Called("ip -4 rule del pref 30201 unreachable") || f.Called("del pref 30200 iif") {
			t.Errorf("calls:\n%s", strings.Join(f.Calls, "\n"))
		}
	})
}

func TestUplinkStatusAndRuleStatus(t *testing.T) {
	f := sys.NewFake()
	f.On("ip -d -j addr show", `[{"ifname":"lo","addr_info":[]},{"ifname":"ve-bare","addr_info":[{"family":"inet","local":"169.254.100.0","prefixlen":31}]}]`)
	f.On("nft list chain ip nsm ve-bare", "table ip nsm {\n\tchain ve-bare {\n\t\tip saddr 169.254.100.1 oifname != \"ve-bare\" masquerade\n\t}\n}")
	f.On("ip -n bare -j -4 route show default", `[{"dst":"default","gateway":"169.254.100.0","dev":"ve0","flags":[]}]`)
	f.On("ip -j -4 rule show", pairJSON(30200, "main"))
	c := uplinkConn(&conn.Rule{Pref: 30200, Table: sys.Main})
	st, err := UplinkStatus(f.Sys, c)
	if err != nil || !st.OK() || st.String() != "ve-bare 169.254.100.0/31, nat, default via 169.254.100.0 dev ve0" {
		t.Errorf("uplink status = %q, %v (%+v)", st, err, st)
	}
	rs, err := RuleStatus(f.Sys, c)
	if err != nil || len(rs) != 2 || !rs[0].Present || !rs[1].Present || rs[0].String() != "30200 lookup main" || rs[1].String() != "30201 unreachable" {
		t.Errorf("rule status = %v, %v", rs, err)
	}

	// pieces missing: each one is named
	f = sys.NewFake()
	f.On("ip -d -j addr show", `[{"ifname":"lo","addr_info":[]}]`)
	f.On("ip -n bare -j -4 route show default", `[{"dst":"default","gateway":"10.0.0.1","dev":"tun0","flags":[]}]`)
	f.On("ip -j -4 rule show", `[{"priority":30201,"src":"all","iif":"ve-bare","action":"unreachable"}]`)
	st, err = UplinkStatus(f.Sys, c)
	if err != nil || st.OK() || st.String() != "veth missing in main, no NAT chain, no default route through ve0" {
		t.Errorf("broken uplink = %q, %v", st, err)
	}
	rs, err = RuleStatus(f.Sys, c)
	if err != nil || rs[0].Present || !rs[1].Present || rs[0].String() != "30200 lookup main MISSING" {
		t.Errorf("rule status = %v, %v", rs, err)
	}
}

func TestMountKeepsTheUplinksVeth(t *testing.T) {
	f := sys.NewFake()
	c := uplinkConn(nil)
	if _, err := Mount(f.Sys, c, pool); err != nil {
		t.Fatal(err)
	}
	if f.Called("ip link del ve-bare") {
		t.Errorf("the veth of an uplink is not a leftover mount:\n%s", strings.Join(f.Calls, "\n"))
	}
	// a mount on a plain connection still unmounts
	f = sys.NewFake()
	if _, err := Mount(f.Sys, &conn.Conn{Name: "bare"}, pool); err != nil {
		t.Fatal(err)
	}
	if !f.Called("ip link del ve-bare") {
		t.Error("plain connection: stale veth must go")
	}
}
