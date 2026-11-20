package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/akovalenko/nsm/internal/sys"
)

// cmdTunfd is the fd-passing pre-exec for tunnel daemons that must live in
// one network namespace while their tun/tap device (addresses, default
// route) sits inside another: attach to the persistent device inside the
// target namespace, return, drop privileges, and exec the daemon with the
// descriptor inherited (`nsm tunfd ... -- calltun accept --link ct0`).
// Exec-inheritance rather than SCM_RIGHTS: with a static topology a broker
// buys nothing, the systemd unit replays the chain on every Restart=always
// by itself, the fd lives exactly as long as the process, and PID tracking
// survives (exec replaces the image in the same PID).
func (g *globals) cmdTunfd(args []string) int {
	fs := flag.NewFlagSet("tunfd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	netnsFlag := fs.String("netns", "", "target network namespace: /run/netns path or bare name (\"main\" = stay here)")
	devFlag := fs.String("dev", "", "persistent tun/tap device to attach to")
	modeFlag := fs.String("mode", "", "tun | tap (default: inferred from the device name prefix)")
	uidFlag := fs.String("uid", "", "drop to this user (name or uid) before exec; default stays root")
	fdEnv := fs.String("fd-env", "", "environment variable the fd number is passed through (required)")
	mtuEnv := fs.String("mtu-env", "", "environment variable the device MTU is passed through")
	if err := fs.Parse(args); err != nil {
		return g.fail("tunfd: %v\n  tunfd --netns NS --dev DEV [--mode tun|tap] [--uid USER] --fd-env NAME [--mtu-env NAME] -- cmd args...", err)
	}
	cmdline := fs.Args()
	switch {
	case *netnsFlag == "":
		return g.fail("tunfd: --netns is required")
	case *devFlag == "":
		return g.fail("tunfd: --dev is required")
	case *fdEnv == "":
		return g.fail("tunfd: --fd-env is required (the daemon learns the descriptor number from it)")
	case len(cmdline) == 0:
		return g.fail("tunfd: no command after --")
	case *fdEnv == *mtuEnv:
		return g.fail("tunfd: --fd-env and --mtu-env must differ (one would clobber the other)")
	}
	if err := g.needRoot("tunfd"); err != nil {
		return g.fail("%v", err)
	}
	tap, err := inferMode(*modeFlag, *devFlag)
	if err != nil {
		return g.fail("tunfd: %v", err)
	}
	nsPath := *netnsFlag
	if !strings.HasPrefix(nsPath, "/") {
		if nsPath == sys.Main {
			nsPath = "/proc/self/ns/net" // setns to our own ns: a permitted no-op
		} else {
			nsPath = filepath.Join(sys.NetnsDir, nsPath)
		}
	}
	var drop *user.User
	if *uidFlag != "" {
		if drop, err = lookupUser(*uidFlag); err != nil {
			return g.fail("tunfd: %v", err)
		}
	}
	path := sys.Which(cmdline[0])

	if g.s.DryRun {
		uid := "root"
		if drop != nil {
			uid = drop.Username
		}
		mode := "tun"
		if tap {
			mode = "tap"
		}
		fmt.Fprintf(g.err, "+ setns %s; attach %s (%s); read mtu there; setns back; setuid %s; %s=<fd> %s=<mtu>; exec %s\n",
			nsPath, *devFlag, mode, uid, *fdEnv, *mtuEnv, strings.Join(cmdline, " "))
		return 0
	}

	env := os.Environ()
	if drop != nil {
		env = setEnv(env, "USER", drop.Username)
		env = setEnv(env, "LOGNAME", drop.Username)
		env = setEnv(env, "HOME", drop.HomeDir)
	}
	err = tunfdExec(nsPath, *devFlag, tap, drop, path, cmdline, env, *fdEnv, *mtuEnv)
	if err != nil {
		return g.fail("tunfd: %v", err)
	}
	return 0 // not reached: success execs
}

// inferMode maps the --mode flag to TAP-or-not, defaulting from the device
// name prefix the way persistent devices are conventionally named.
func inferMode(mode, dev string) (bool, error) {
	switch strings.ToLower(mode) {
	case "":
		switch {
		case strings.HasPrefix(dev, "tun"):
			return false, nil
		case strings.HasPrefix(dev, "tap"):
			return true, nil
		}
		return false, fmt.Errorf("cannot infer tun/tap from device %q; pass --mode", dev)
	case "tun":
		return false, nil
	case "tap":
		return true, nil
	}
	return false, fmt.Errorf("--mode must be tun or tap, got %q", mode)
}

// lookupUser resolves a --uid value: a name, or a numeric uid. Unknown
// names and ids are refused rather than guessed.
func lookupUser(v string) (*user.User, error) {
	if u, err := user.Lookup(v); err == nil {
		return u, nil
	}
	if _, err := strconv.Atoi(v); err == nil {
		return user.LookupId(v)
	}
	return nil, fmt.Errorf("--uid %q: no such user", v)
}

// tunfdExec is the namespace dance, all on one locked thread: setns
// switches the calling THREAD's namespace (an unpinned goroutine could
// migrate between the setns and the ioctl and run them in different
// namespaces), and execve inherits the namespace of the thread that calls
// it — so the same goroutine must also return to the original namespace
// before exec. Descriptor numbers are delivered as they are: fd 3 is not
// free to take via dup2 in a Go process (the runtime keeps its epoll and
// eventfd there), and dup2(x, x) would leave CLOEXEC set.
func tunfdExec(nsPath, dev string, tap bool, drop *user.User, path string, argv, env []string, fdEnv, mtuEnv string) error {
	runtime.LockOSThread()

	// Reference for the way back; CLOEXEC closes it in the exec even on
	// error paths that return instead.
	back, err := unix.Open("/proc/self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /proc/self/ns/net: %w", err)
	}
	defer unix.Close(back)

	target, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s (namespace up?): %w", nsPath, err)
	}
	if err := unix.Setns(target, unix.CLONE_NEWNET); err != nil {
		unix.Close(target)
		return fmt.Errorf("setns %s (root?): %w", nsPath, err)
	}
	unix.Close(target)

	tfd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	if err := tunSetIFF(tfd, dev, tap); err != nil {
		unix.Close(tfd)
		return err
	}
	// /sys/class/net is namespace-tagged: the MTU is only readable here,
	// inside the namespace that owns the device — the daemon will sit
	// outside and cannot re-check it itself.
	mtu, err := readDevMTU(dev)
	if err != nil {
		unix.Close(tfd)
		return fmt.Errorf("attach %s: %w", dev, err)
	}

	if err := unix.Setns(back, unix.CLONE_NEWNET); err != nil {
		unix.Close(tfd)
		return fmt.Errorf("setns back to the original namespace: %w", err)
	}

	// Drop before handing the descriptor over: the daemon then holds no
	// path back to root. Supplementary groups are cleared, not inherited
	// (a daemon needs its own uid, not the login group set).
	if drop != nil {
		uid, _ := strconv.Atoi(drop.Uid)
		gid, _ := strconv.Atoi(drop.Gid)
		if err := syscall.Setgroups(nil); err != nil {
			unix.Close(tfd)
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := syscall.Setgid(gid); err != nil {
			unix.Close(tfd)
			return fmt.Errorf("setgid: %w", err)
		}
		if err := syscall.Setuid(uid); err != nil {
			unix.Close(tfd)
			return fmt.Errorf("setuid: %w", err)
		}
	}

	// Deliver the fd: clear CLOEXEC on exactly this descriptor...
	if _, err := unix.FcntlInt(uintptr(tfd), unix.F_SETFD, 0); err != nil {
		unix.Close(tfd)
		return fmt.Errorf("clear CLOEXEC on the tun fd: %w", err)
	}
	// ...and tell the daemon where it is.
	env = setEnv(env, fdEnv, strconv.Itoa(tfd))
	if mtuEnv != "" {
		env = setEnv(env, mtuEnv, strconv.Itoa(mtu))
	}
	return syscall.Exec(path, argv, env)
}

// tunSetIFF attaches fd to the named persistent device inside the current
// network namespace.
func tunSetIFF(fd int, dev string, tap bool) error {
	flags := uint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if tap {
		flags = uint16(unix.IFF_TAP | unix.IFF_NO_PI)
	}
	var ifr [unix.IFNAMSIZ + 2]byte
	copy(ifr[:unix.IFNAMSIZ-1], dev)
	*(*uint16)(unsafe.Pointer(&ifr[unix.IFNAMSIZ])) = flags
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TUNSETIFF, uintptr(unsafe.Pointer(&ifr[0])))
	if errno != 0 {
		switch errno {
		case unix.EPERM:
			return fmt.Errorf("attach %s: EPERM (attaching to an existing persistent device needs no caps; EPERM usually means it is missing and creating it here lacks CAP_NET_ADMIN)", dev)
		case unix.ENODEV:
			return fmt.Errorf("attach %s: no such device in this namespace (create it there first: ip -n <ns> tuntap add)", dev)
		default:
			return fmt.Errorf("attach %s: %w", dev, errno)
		}
	}
	return nil
}

// readDevMTU reads the device MTU from sysfs, valid only inside the
// namespace that owns the device.
func readDevMTU(dev string) (int, error) {
	b, err := os.ReadFile("/sys/class/net/" + dev + "/mtu")
	if err != nil {
		return 0, err
	}
	mtu, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || mtu <= 0 {
		return 0, fmt.Errorf("sysfs mtu %q: not a number", string(b))
	}
	return mtu, nil
}
