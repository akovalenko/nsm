package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/akovalenko/nsm/internal/sys"
)

func TestInferMode(t *testing.T) {
	for _, tc := range []struct {
		mode, dev string
		tap       bool
		wantErr   string
	}{
		{mode: "tun", dev: "whatever", tap: false},
		{mode: "tap", dev: "whatever", tap: true},
		{mode: "", dev: "tun44", tap: false},
		{mode: "", dev: "tap44", tap: true},
		{mode: "", dev: "ct0", wantErr: "cannot infer"},
		{mode: "TUN", dev: "tun44", tap: false},
		{mode: "ipsec", dev: "tun44", wantErr: "must be tun or tap"},
	} {
		tap, err := inferMode(tc.mode, tc.dev)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("inferMode(%q,%q): want error %q, got %v", tc.mode, tc.dev, tc.wantErr, err)
			}
			continue
		}
		if err != nil || tap != tc.tap {
			t.Errorf("inferMode(%q,%q) = %v, %v; want %v, nil", tc.mode, tc.dev, tap, err, tc.tap)
		}
	}
}

func TestLookupUser(t *testing.T) {
	if u, err := lookupUser("root"); err != nil || u.Uid != "0" {
		t.Errorf("lookupUser(root) = %v, %v", u, err)
	}
	if u, err := lookupUser("0"); err != nil || u.Username != "root" {
		t.Errorf("lookupUser(0) = %v, %v", u, err)
	}
	if _, err := lookupUser("no-such-user-xyzzy"); err == nil {
		t.Error("lookupUser(garbage): want an error")
	}
}

// The flag surface of tunfd: what must be refused before anything
// namespace-touching happens, and that a complete dry run plans cleanly.
func TestCmdTunfdValidation(t *testing.T) {
	base := []string{"--netns", "wgedge", "--dev", "tun44", "--fd-env", "CALLTUN_FD", "--", "/bin/sleep", "1"}
	for _, tc := range []struct {
		name    string
		args    []string
		wantOut int // exit status
	}{
		{name: "ok-dryrun", args: base, wantOut: 0},
		{name: "no-fd-env", args: []string{"--netns", "x", "--dev", "tun44", "--", "/bin/true"}, wantOut: 1},
		{name: "no-netns", args: []string{"--dev", "tun44", "--fd-env", "F", "--", "/bin/true"}, wantOut: 1},
		{name: "no-dev", args: []string{"--netns", "x", "--fd-env", "F", "--", "/bin/true"}, wantOut: 1},
		{name: "no-command", args: []string{"--netns", "x", "--dev", "tun44", "--fd-env", "F"}, wantOut: 1},
		{name: "bad-mode", args: append([]string{"--mode", "ipsec"}, base...), wantOut: 1},
		{name: "bare-name-mtu", args: append([]string{"--mtu-env", "CALLTUN_MTU", "--uid", "root"}, base...), wantOut: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			g := &globals{out: &out, err: &out, s: sys.New()}
			g.s.DryRun = true
			if rc := g.cmdTunfd(tc.args); rc != tc.wantOut {
				t.Fatalf("exit %d, want %d; output:\n%s", rc, tc.wantOut, out.String())
			}
			if tc.wantOut == 0 && !strings.Contains(out.String(), "attach tun44") {
				t.Fatalf("plan line missing attach; output:\n%s", out.String())
			}
		})
	}
}
