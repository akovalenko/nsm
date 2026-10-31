package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akovalenko/nsm/internal/conn"
	"github.com/akovalenko/nsm/internal/expose"
	"github.com/akovalenko/nsm/internal/helper"
	"github.com/akovalenko/nsm/internal/nanny"
	"github.com/akovalenko/nsm/internal/netns"
)

func writePid(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// readPid returns the pid in path if that process is alive and is an nsm
// process (so a recycled pid is never signalled), else 0.
func readPid(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0
	}
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		return 0
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !strings.Contains(string(cmdline), "nsm") {
		return 0
	}
	return pid
}

// stopPid terminates the process recorded in path, escalating to SIGKILL.
func stopPid(path string, grace time.Duration) {
	pid := readPid(path)
	if pid == 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func (g *globals) selfArgs(cmd, name string) []string {
	args := []string{"-C", g.confdir, "-pool", g.pool.String(), "-wait", g.wait.String()}
	if g.s.Verbose {
		args = append(args, "-v")
	}
	if g.anyNS {
		args = append(args, "-any-ns")
	}
	return append(args, cmd, name)
}

// cmdUp detaches a nanny around `nsm run <name>`; its output goes to
// /run/nsm/<name>/log.
func (g *globals) cmdUp(name string) int {
	if err := g.needRoot("up"); err != nil {
		return g.fail("%v", err)
	}
	if err := g.requireMain("up"); err != nil {
		return g.fail("%v", err)
	}
	if _, err := conn.Load(g.confdir, name); err != nil {
		return g.fail("%v", err)
	}
	stateDir := filepath.Join(helper.RunDir, name)
	if pid := readPid(filepath.Join(stateDir, "nanny.pid")); pid != 0 {
		fmt.Fprintf(g.out, "%s: already up (nanny pid %d)\n", name, pid)
		return 0
	}
	if g.s.DryRun {
		fmt.Fprintf(g.err, "+ nsm %s &\n", strings.Join(g.selfArgs("nanny", name), " "))
		return 0
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return g.fail("%v", err)
	}
	logPath := filepath.Join(stateDir, "log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return g.fail("%v", err)
	}
	defer logf.Close()
	self, err := os.Executable()
	if err != nil {
		return g.fail("%v", err)
	}
	cmd := exec.Command(self, g.selfArgs("nanny", name)...)
	cmd.Stdin = nil
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return g.fail("start nanny: %v", err)
	}
	fmt.Fprintf(g.out, "%s: nanny pid %d, log %s\n", name, cmd.Process.Pid, logPath)
	return 0
}

// cmdNanny is the foreground respawn loop (what `up` detaches).
func (g *globals) cmdNanny(name string) int {
	if err := g.needRoot("nanny"); err != nil {
		return g.fail("%v", err)
	}
	if err := g.requireMain("nanny"); err != nil {
		return g.fail("%v", err)
	}
	if _, err := conn.Load(g.confdir, name); err != nil {
		return g.fail("%v", err)
	}
	stateDir := filepath.Join(helper.RunDir, name)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return g.fail("%v", err)
	}
	pidfile := filepath.Join(stateDir, "nanny.pid")
	if pid := readPid(pidfile); pid != 0 && pid != os.Getpid() {
		return g.fail("%s: nanny already running (pid %d)", name, pid)
	}
	if err := writePid(pidfile, os.Getpid()); err != nil {
		return g.fail("%v", err)
	}
	defer os.Remove(pidfile)
	self, err := os.Executable()
	if err != nil {
		return g.fail("%v", err)
	}
	if err := nanny.Run(append([]string{self}, g.selfArgs("run", name)...), nanny.Options{Log: g.err}); err != nil {
		return g.fail("%v", err)
	}
	return 0
}

// cmdDown stops whatever runs the connection and removes its skeleton:
// mounts, veth, namespace (which takes the payload interface with it),
// resolv.conf override and volatile state. Idempotent.
func (g *globals) cmdDown(name string) int {
	if err := g.needRoot("down"); err != nil {
		return g.fail("%v", err)
	}
	if err := g.requireMain("down"); err != nil {
		return g.fail("%v", err)
	}
	c, err := conn.Load(g.confdir, name)
	if err != nil {
		if !conn.ValidName(name) {
			return g.fail("%v", err)
		}
		// Directory gone but kernel state may remain: tear down by name.
		c = &conn.Conn{Name: name}
	}
	stateDir := filepath.Join(helper.RunDir, name)
	if !g.s.DryRun {
		stopPid(filepath.Join(stateDir, "nanny.pid"), 15*time.Second)
		stopPid(filepath.Join(stateDir, "run.pid"), 20*time.Second)
	}
	expose.Unmount(g.s, c)
	netns.Delete(g.s, c.NS())
	if !g.s.DryRun {
		os.RemoveAll(stateDir)
	}
	return 0
}
