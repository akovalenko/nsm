package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/akovalenko/nsm/internal/conn"
	"github.com/akovalenko/nsm/internal/netns"
)

// cmdExec enters the connection's netns and drops to the invoking user: the
// classic privileged-setns-then-setuid entry (what ip netns exec + runuser,
// vopono and firejail do). Besides the netns it gives the command a private
// mount ns with the connection's resolv.conf, like `ip netns exec` does.
func (g *globals) cmdExec(args []string, shell bool) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	userFlag := fs.String("u", "", "")
	if len(args) == 0 {
		return g.fail("exec: want <name> [-u user] cmd...")
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return g.fail("exec: %v", err)
	}
	cmdline := fs.Args()
	if !conn.ValidName(name) {
		return g.fail("exec: invalid connection name %q", name)
	}
	if os.Geteuid() != 0 {
		return g.fail("exec needs root: sudo nsm exec %s ...", name)
	}
	if !netns.Exists(name) {
		return g.fail("exec: netns %s does not exist (connection not up?)", name)
	}
	target, err := targetUser(*userFlag)
	if err != nil {
		return g.fail("exec: %v", err)
	}
	if shell {
		if len(cmdline) != 0 {
			return g.fail("shell takes no command")
		}
		sh := "/bin/sh"
		if target != nil {
			if s := loginShell(target.Username); s != "" {
				sh = s
			}
		} else if s := os.Getenv("SHELL"); s != "" {
			sh = s
		}
		cmdline = []string{sh, "-l"}
	}
	if len(cmdline) == 0 {
		return g.fail("exec: no command given")
	}
	env := os.Environ()
	env = setEnv(env, "NSM_CONN", name)
	if target != nil {
		env = userEnv(env, target)
	}
	path, err := exec.LookPath(cmdline[0])
	if err != nil {
		return g.fail("exec: %v", err)
	}
	if g.s.DryRun {
		fmt.Fprintf(g.err, "+ setns %s; bind resolv.conf; setuid %s; exec %s\n", netns.Path(name), userName(target), strings.Join(cmdline, " "))
		return 0
	}
	if err := enter(name, target, path, cmdline, env); err != nil {
		return g.fail("exec: %v", err)
	}
	return 0 // not reached: enter execs
}

func userName(u *user.User) string {
	if u == nil {
		return "root"
	}
	return u.Username
}

// resolverSockets are the unix sockets through which NSS daemons in main
// would resolve on the namespace's behalf, defeating the connection's own
// resolv.conf (per-netns nsswitch.conf does not help: the nscd interception
// in glibc happens before nsswitch).
var resolverSockets = []string{
	"/var/run/nscd/socket",                    // nscd hosts cache
	"/run/systemd/resolve/io.systemd.Resolve", // nss-resolve → systemd-resolved
	"/var/lib/sss/pipes/nss",                  // sssd NSS responder
}

// targetUser resolves -u, else the sudo caller, else nil (stay root).
func targetUser(flag string) (*user.User, error) {
	if flag != "" {
		if u, err := user.Lookup(flag); err == nil {
			return u, nil
		}
		return user.LookupId(flag)
	}
	if uid := os.Getenv("SUDO_UID"); uid != "" {
		return user.LookupId(uid)
	}
	return nil, nil
}

// enter does the namespace dance on one locked thread and then replaces the
// process; nothing after a successful Exec runs.
func enter(name string, target *user.User, path string, argv, env []string) error {
	runtime.LockOSThread()
	fd, err := syscall.Open(netns.Path(name), syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", netns.Path(name), err)
	}
	if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("setns %s: %w", name, err)
	}
	syscall.Close(fd)

	// Private mount ns so the resolv.conf bind does not leak to the host.
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		return fmt.Errorf("unshare mount ns: %w", err)
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("make / rslave: %w", err)
	}
	resolv := filepath.Join(netns.EtcDir, name, "resolv.conf")
	if _, err := os.Stat(resolv); err == nil {
		if err := syscall.Mount(resolv, "/etc/resolv.conf", "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s: %w", resolv, err)
		}
	}

	// Shadow resolver-daemon sockets. Filesystem unix sockets cross netns
	// boundaries, so nscd or systemd-resolved in main would keep answering
	// glibc's lookups on the connection's behalf — resolving with main's
	// resolver and main's knowledge, negative answers included, and never
	// letting the connection's resolv.conf speak. A /dev/null bind makes
	// their connect() fail and glibc falls back to nsswitch and the dns
	// module with OUR resolv.conf.
	for _, p := range resolverSockets {
		if _, err := os.Stat(p); err == nil {
			if err := unix.Mount("/dev/null", p, "", unix.MS_BIND, ""); err != nil {
				return fmt.Errorf("shadow %s: %w", p, err)
			}
		}
	}

	if target != nil {
		uid, _ := strconv.Atoi(target.Uid)
		gid, _ := strconv.Atoi(target.Gid)
		gids := []int{gid}
		if ids, err := target.GroupIds(); err == nil {
			for _, s := range ids {
				if n, err := strconv.Atoi(s); err == nil && n != gid {
					gids = append(gids, n)
				}
			}
		}
		if err := syscall.Setgroups(gids); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := syscall.Setgid(gid); err != nil {
			return fmt.Errorf("setgid: %w", err)
		}
		if err := syscall.Setuid(uid); err != nil {
			return fmt.Errorf("setuid: %w", err)
		}
	}
	return syscall.Exec(path, argv, env)
}

// userEnv repairs what sudo's env_reset took away so desktop apps find
// their session: HOME/USER and, if absent and present on disk, the
// XDG runtime dir, Wayland socket and session bus. DISPLAY survives sudo.
func userEnv(env []string, u *user.User) []string {
	env = setEnv(env, "HOME", u.HomeDir)
	env = setEnv(env, "USER", u.Username)
	env = setEnv(env, "LOGNAME", u.Username)
	if sh := loginShell(u.Username); sh != "" {
		env = setEnv(env, "SHELL", sh)
	}
	rt := "/run/user/" + u.Uid
	if _, err := os.Stat(rt); err != nil {
		return env
	}
	if getEnv(env, "XDG_RUNTIME_DIR") == "" {
		env = setEnv(env, "XDG_RUNTIME_DIR", rt)
	}
	if getEnv(env, "WAYLAND_DISPLAY") == "" {
		if _, err := os.Stat(filepath.Join(rt, "wayland-0")); err == nil {
			env = setEnv(env, "WAYLAND_DISPLAY", "wayland-0")
		}
	}
	if getEnv(env, "DBUS_SESSION_BUS_ADDRESS") == "" {
		if _, err := os.Stat(filepath.Join(rt, "bus")); err == nil {
			env = setEnv(env, "DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(rt, "bus"))
		}
	}
	return env
}

func getEnv(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func setEnv(env []string, key, val string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+val)
}

// loginShell reads the shell field of /etc/passwd (os/user does not expose it).
func loginShell(username string) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 7 && fields[0] == username {
			return fields[6]
		}
	}
	return ""
}
