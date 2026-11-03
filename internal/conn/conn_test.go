package conn

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseMeta(t *testing.T) {
	src := `
# comment
rides=lappy
expose="10.48.0.0/16,10.10.0.5/24 10.48.0.9/16"   # commas, spaces, dup collapsed
expose=192.168.90.0/24
resolv="nameserver 1.1.1.1"
resolv="nameserver 1.0.0.1"   # dup below collapses
resolv="options edns0 trust-ad"
resolv="nameserver 1.0.0.1"
mtu=1370
`
	m, err := ParseMeta(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if m.Rides != "lappy" {
		t.Errorf("rides = %q", m.Rides)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.48.0.0/16"), netip.MustParsePrefix("10.10.0.0/24"), netip.MustParsePrefix("192.168.90.0/24")}
	if !slices.Equal(m.Expose, want) {
		t.Errorf("expose = %v, want %v", m.Expose, want)
	}
	if !slices.Equal(m.Resolv, []string{"nameserver 1.1.1.1", "nameserver 1.0.0.1", "options edns0 trust-ad"}) || m.MTU != 1370 {
		t.Errorf("resolv=%q mtu=%d", m.Resolv, m.MTU)
	}
	c := &Conn{Name: "t", Dir: t.TempDir(), Meta: m}
	wantRC := "nameserver 1.1.1.1\nnameserver 1.0.0.1\noptions edns0 trust-ad\n"
	if rc, err := c.ResolvConf(); err != nil || rc != wantRC {
		t.Errorf("ResolvConf = %q, %v", rc, err)
	}
}

func TestResolvConf(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "conf", "resolv.conf")
	content := "nameserver 10.48.1.53\nnameserver 10.48.1.54\nsearch eputs.spd" // no trailing NL on purpose
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	mk := func(v ...string) *Conn { return &Conn{Name: "t", Dir: dir, Meta: Meta{Resolv: v}} }

	// relative path with a slash resolves against the connection dir
	rc, err := mk("conf/resolv.conf").ResolvConf()
	if err != nil || rc != content+"\n" {
		t.Errorf("relative file: %q, %v", rc, err)
	}
	// so does an absolute path
	rc, err = mk(file).ResolvConf()
	if err != nil || rc != content+"\n" {
		t.Errorf("absolute file: %q, %v", rc, err)
	}
	// a literal directive passes; anything else is refused loudly
	if rc, err := mk("nameserver 1.1.1.1").ResolvConf(); err != nil || rc != "nameserver 1.1.1.1\n" {
		t.Errorf("literal: %q, %v", rc, err)
	}
	if rc, err := mk("options edns0 trust-ad").ResolvConf(); err != nil || rc != "options edns0 trust-ad\n" {
		t.Errorf("options literal: %q, %v", rc, err)
	}
	if _, err := mk("resolv.txt").ResolvConf(); err == nil {
		t.Error("bare name without / must not be treated as a literal or a file")
	}
	if _, err := mk("conf/missing").ResolvConf(); err == nil {
		t.Error("missing file must fail")
	}
	if rc, err := mk("").ResolvConf(); err != nil || rc != "" {
		t.Errorf("empty: %q, %v", rc, err)
	}
	// several pieces accumulate in order, files and literals mixed
	rc, err = mk("nameserver 9.9.9.9", "conf/resolv.conf", "options rotate").ResolvConf()
	if err != nil || rc != "nameserver 9.9.9.9\n"+content+"\noptions rotate\n" {
		t.Errorf("multi: %q, %v", rc, err)
	}
}

func TestParseMetaDefaultsAndErrors(t *testing.T) {
	m, err := ParseMeta(strings.NewReader(""))
	if err != nil || m.Rides != "main" || m.Expose != nil || m.MTU != 0 {
		t.Errorf("empty meta: %+v, %v", m, err)
	}
	for _, bad := range []string{
		"bogus=1",
		"expose=10.0.0.0/8 garbage",
		"expose=fd00::/8",
		"mtu=100",
		"dns=nodomain",
		"rides='unterminated",
		"rides=main.too.long.name",
		"just a line",
	} {
		if _, err := ParseMeta(strings.NewReader(bad)); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"lappy", "client59", "a", "wg-office.2", "twelve_chars"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "main", "-lead", "thirteen_char", "with space", "../x"} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestLoadAndList(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, meta string) {
		d := filepath.Join(dir, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if meta != "" {
			if err := os.WriteFile(filepath.Join(d, "meta"), []byte(meta), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("lappy", "")
	mk("client59", "rides=lappy\nexpose=10.48.0.0/16\n")
	mk("selfish", "rides=selfish\n")
	if err := os.WriteFile(filepath.Join(dir, "stray-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := List(dir)
	if err != nil || strings.Join(names, ",") != "client59,lappy,selfish" {
		t.Errorf("List = %v, %v", names, err)
	}
	c, err := Load(dir, "client59")
	if err != nil {
		t.Fatal(err)
	}
	if c.Meta.Rides != "lappy" || c.NS() != "client59" || c.Veth() != "ve-client59" || c.RunScript() != filepath.Join(dir, "client59", "run") {
		t.Errorf("Load = %+v", c)
	}
	if c, err := Load(dir, "lappy"); err != nil || c.Meta.Rides != "main" {
		t.Errorf("lappy: %+v, %v", c, err)
	}
	if _, err := Load(dir, "selfish"); err == nil {
		t.Error("rides=self should fail")
	}
	if _, err := Load(dir, "nope"); err == nil {
		t.Error("missing should fail")
	}
	if _, err := Load(dir, "main"); err == nil {
		t.Error("main should be rejected")
	}
	if l, err := List(filepath.Join(dir, "absent")); err != nil || l != nil {
		t.Errorf("absent confdir: %v, %v", l, err)
	}
}
