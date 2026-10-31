// Package conn reads connection directories: <confdir>/<name>/ with a
// mandatory `run` script, an optional `meta` stanza and whatever native
// artifacts the transport wants under conf/. nsm never looks inside conf/.
package conn

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/akovalenko/nsm/internal/sys"
)

// DefaultConfDir is where connections live unless -C says otherwise.
const DefaultConfDir = "/etc/nsm"

// DefaultPool is the link-local block veth /31s are carved from.
const DefaultPool = "169.254.100.0/23"

// Names double as netns names and as the stem of the `ve-<name>` veth, so
// they must fit an interface name (15 chars) with the prefix.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,11}$`)

// ValidName reports whether name may be used as a connection name.
func ValidName(name string) bool { return nameRE.MatchString(name) && name != sys.Main }

// DNSEntry is one `domain:server` pair of the dns= key.
type DNSEntry struct {
	Domain string
	Server netip.Addr
}

// Meta is the parsed `meta` stanza; zero values are the defaults.
type Meta struct {
	Rides  string         // netns carrying the transport socket; sys.Main by default
	Expose []netip.Prefix // prefixes to mount into main
	DNS    []DNSEntry     // domain-scoped forwarders to publish into main dnsmasq
	Resolv string         // resolv.conf for this connection's ns (literal or file content)
	MTU    int            // payload interface MTU override
}

// Conn is a loaded connection directory.
type Conn struct {
	Name string
	Dir  string
	Meta Meta
}

// NS is the connection's own network namespace name.
func (c *Conn) NS() string { return c.Name }

// Veth is the main-side veth name of the expose mount.
func (c *Conn) Veth() string { return "ve-" + c.Name }

// RunScript is the path of the mandatory run script.
func (c *Conn) RunScript() string { return filepath.Join(c.Dir, "run") }

// Load reads <confdir>/<name>.
func Load(confdir, name string) (*Conn, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid connection name %q (1-12 chars of [A-Za-z0-9_.-], not %q)", name, sys.Main)
	}
	dir := filepath.Join(confdir, name)
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("connection %s: %w", name, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("connection %s: %s is not a directory", name, dir)
	}
	c := &Conn{Name: name, Dir: dir, Meta: Meta{Rides: sys.Main}}
	f, err := os.Open(filepath.Join(dir, "meta"))
	switch {
	case err == nil:
		defer f.Close()
		m, err := ParseMeta(f)
		if err != nil {
			return nil, fmt.Errorf("connection %s: meta: %w", name, err)
		}
		c.Meta = m
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("connection %s: %w", name, err)
	}
	if c.Meta.Rides == c.Name {
		return nil, fmt.Errorf("connection %s: rides itself", name)
	}
	return c, nil
}

// List returns the connection names under confdir, sorted. A missing
// confdir is an empty list, not an error.
func List(confdir string) ([]string, error) {
	ents, err := os.ReadDir(confdir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() && ValidName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ParseMeta parses the shell-sourceable key=value stanza. Values may be
// bare or in single/double quotes; `#` starts a comment; unknown keys are an
// error so typos do not silently become defaults.
func ParseMeta(r io.Reader) (Meta, error) {
	m := Meta{Rides: sys.Main}
	sc := bufio.NewScanner(r)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return m, fmt.Errorf("line %d: expected key=value", ln)
		}
		key = strings.TrimSpace(key)
		val, err := unquote(strings.TrimSpace(val))
		if err != nil {
			return m, fmt.Errorf("line %d: %v", ln, err)
		}
		switch key {
		case "rides":
			if val == "" {
				val = sys.Main
			}
			if val != sys.Main && !ValidName(val) {
				return m, fmt.Errorf("line %d: rides=%q is not a connection name", ln, val)
			}
			m.Rides = val
		case "expose":
			for _, f := range metaFields(val) {
				p, err := netip.ParsePrefix(f)
				if err != nil {
					return m, fmt.Errorf("line %d: expose %q: %v", ln, f, err)
				}
				if !p.Addr().Is4() {
					return m, fmt.Errorf("line %d: expose %q: IPv4 only for now", ln, f)
				}
				p = p.Masked()
				if !slices.Contains(m.Expose, p) {
					m.Expose = append(m.Expose, p)
				}
			}
		case "dns":
			for _, f := range metaFields(val) {
				dom, srv, ok := strings.Cut(f, ":")
				if !ok || dom == "" {
					return m, fmt.Errorf("line %d: dns %q: want domain:server", ln, f)
				}
				a, err := netip.ParseAddr(srv)
				if err != nil {
					return m, fmt.Errorf("line %d: dns %q: %v", ln, f, err)
				}
				m.DNS = append(m.DNS, DNSEntry{Domain: strings.TrimSuffix(dom, "."), Server: a})
			}
		case "resolv":
			m.Resolv = val
		case "mtu":
			n, err := strconv.Atoi(val)
			if err != nil || n < 576 || n > 65535 {
				return m, fmt.Errorf("line %d: mtu %q: want an integer in 576..65535", ln, val)
			}
			m.MTU = n
		default:
			return m, fmt.Errorf("line %d: unknown key %q", ln, key)
		}
	}
	return m, sc.Err()
}

// metaFields splits list-valued meta values: commas and whitespace, both,
// like dns= has always done.
func metaFields(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
}

// unquote strips one level of shell quotes; in double quotes only \" and \\
// are escapes, which is enough for the handful of keys meta carries.
func unquote(v string) (string, error) {
	if v == "" {
		return v, nil
	}
	q := v[0]
	if q != '\'' && q != '"' {
		if i := strings.IndexByte(v, '#'); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		return v, nil
	}
	// Find the closing quote; inside double quotes \" is an escape.
	end := -1
	for i := 1; i < len(v); i++ {
		if q == '"' && v[i] == '\\' {
			i++
			continue
		}
		if v[i] == q {
			end = i
			break
		}
	}
	if end < 0 {
		return "", fmt.Errorf("unterminated quote")
	}
	if rest := strings.TrimSpace(v[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("trailing garbage after quoted value: %q", rest)
	}
	s := v[1:end]
	if q == '"' {
		s = strings.ReplaceAll(s, `\"`, "\x00")
		s = strings.ReplaceAll(s, `\\`, `\`)
		s = strings.ReplaceAll(s, "\x00", `"`)
	}
	return s, nil
}

// ResolvConf returns the resolv.conf text the connection's ns should get
// from meta, or "" when meta says nothing (the transport may then supply
// pushed DNS). A value naming an existing file means that file's content.
func (m Meta) ResolvConf() (string, error) {
	v := m.Resolv
	if v == "" {
		return "", nil
	}
	if strings.HasPrefix(v, "/") {
		b, err := os.ReadFile(v)
		if err != nil {
			return "", fmt.Errorf("resolv=%s: %w", v, err)
		}
		return string(b), nil
	}
	if !strings.HasSuffix(v, "\n") {
		v += "\n"
	}
	return v, nil
}
