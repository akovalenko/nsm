package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/akovalenko/nsm/internal/conn"
	"github.com/akovalenko/nsm/internal/expose"
	"github.com/akovalenko/nsm/internal/helper"
	"github.com/akovalenko/nsm/internal/netns"
	"github.com/akovalenko/nsm/internal/sys"
)

// cmdRun is the foreground entry a supervisor runs: wait for the substrate,
// build the skeleton (ns, resolv.conf, mounts), start the run script in the
// transport ns, and die when the substrate does.
func (g *globals) cmdRun(name string) int {
	if err := g.needRoot("run"); err != nil {
		return g.fail("%v", err)
	}
	if err := g.requireMain("run"); err != nil {
		return g.fail("%v", err)
	}
	c, err := conn.Load(g.confdir, name)
	if err != nil {
		return g.fail("%v", err)
	}
	if st, err := os.Stat(c.RunScript()); err != nil || st.Mode()&0o111 == 0 {
		return g.fail("%s: run script missing or not executable", c.RunScript())
	}
	s := g.s
	logf := func(f string, a ...any) { fmt.Fprintf(g.err, "nsm run %s: "+f+"\n", append([]any{name}, a...)...) }

	var sub *netns.Substrate
	if s.DryRun && !netns.Exists(c.Meta.Rides) {
		logf("(dry run) would wait for substrate %s", c.Meta.Rides)
		sub = &netns.Substrate{NS: c.Meta.Rides}
	} else {
		sub, err = netns.WaitSubstrate(s, c.Meta.Rides, g.wait)
		if err != nil {
			return g.fail("%v", err)
		}
	}
	if err := netns.Ensure(s, c.NS()); err != nil {
		return g.fail("%v", err)
	}
	rc, err := c.ResolvConf()
	if err != nil {
		return g.fail("%v", err)
	}
	if rc != "" {
		if err := netns.WriteResolv(s, c.NS(), rc); err != nil {
			return g.fail("%v", err)
		}
	}
	results, err := expose.Mount(s, c, g.pool)
	if err != nil {
		return g.fail("expose: %v", err)
	}
	for _, r := range results {
		if !r.Mounted {
			logf("expose %s", r)
		} else if s.Verbose {
			logf("expose %s", r)
		}
	}

	stateDir := filepath.Join(helper.RunDir, name)
	if !s.DryRun {
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return g.fail("%v", err)
		}
		if err := writePid(filepath.Join(stateDir, "run.pid"), os.Getpid()); err != nil {
			return g.fail("%v", err)
		}
		defer os.Remove(filepath.Join(stateDir, "run.pid"))
	}

	if s.DryRun {
		logf("(dry run) would exec %s in netns %s; run scripts are not executed in dry run", c.RunScript(), c.Meta.Rides)
		return 0
	}
	cmd := s.Command(c.Meta.Rides, c.RunScript())
	cmd.Dir = c.Dir
	cmd.Env = runEnv(c, g)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return g.fail("start %s: %v", c.RunScript(), err)
	}
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	stop := func(reason string, code int) int {
		logf("%s; stopping transport", reason)
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			logf("transport ignored SIGTERM, killing")
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
		}
		return code
	}
	for {
		select {
		case sg := <-sig:
			return stop(fmt.Sprintf("got %v", sg), 0)
		case <-tick.C:
			if err := sub.Check(s); err != nil {
				return stop(err.Error(), 1)
			}
		case err := <-done:
			if err == nil {
				logf("transport exited cleanly")
				return 0
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				logf("transport exited: %v", ee)
				if code := ee.ExitCode(); code > 0 {
					return code
				}
				return 1
			}
			return g.fail("%v", err)
		}
	}
}

// runEnv is the environment the run script (and the helpers it execs) see.
func runEnv(c *conn.Conn, g *globals) []string {
	env := append(os.Environ(), helper.Vars(c)...)
	env = append(env, "NSM_CONFDIR="+g.confdir)
	if g.s.Verbose {
		env = append(env, "NSM_VERBOSE=1")
	}
	// Make sure `nsm helper ...` in the run script finds this very binary.
	if self, err := os.Executable(); err == nil {
		env = append(env, "PATH="+filepath.Dir(self)+":"+os.Getenv("PATH"))
	}
	return env
}

// helperWG / helperOpenVPN / helperOpenVPNUp adapt the helper package to the
// CLI; they are exec'd by run scripts inside the transport ns.
func (g *globals) helperWG(conf string) int {
	e, err := helper.FromProcess()
	if err != nil {
		return g.fail("%v", err)
	}
	if err := helper.RunWG(g.s, e, conf); err != nil {
		return g.fail("helper wg: %v", err)
	}
	return 0
}

func (g *globals) helperOpenVPN(ovpn string, extra []string) int {
	e, err := helper.FromProcess()
	if err != nil {
		return g.fail("%v", err)
	}
	err = helper.RunOpenVPN(g.s, e, ovpn, extra)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	return g.fail("helper openvpn: %v", err)
}

func (g *globals) helperOpenVPNUp(args []string) int {
	e, err := helper.FromArgs(args)
	if err != nil {
		return g.fail("%v", err)
	}
	if err := helper.OpenVPNUp(g.s, e, os.Getenv); err != nil {
		return g.fail("helper openvpn-up: %v", err)
	}
	return 0
}

// quiet silences a Sys for probes whose plan lines would be noise.
func quiet(s *sys.Sys) *sys.Sys {
	q := *s
	q.Verbose = false
	return &q
}
