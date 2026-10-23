// Package cli is the nsm command line: global flags, dispatch, help.
package cli

import (
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/akovalenko/nsm/internal/conn"
	"github.com/akovalenko/nsm/internal/sys"
)

const usage = `nsm - netns-first connection manager

usage: nsm [-C confdir] [-v] [-n] [-pool CIDR] [-wait DUR] <command> ...

commands (root):
  run <name>                 foreground: build the skeleton, run the transport
                             (this is what a supervisor should run)
  up <name>                  detach the built-in nanny around 'nsm run'
  down <name>                stop the nanny/run and tear the connection down
  status [name...]           what the kernel says about each connection
  exec <name> [-u user] cmd  run cmd inside the connection's netns as user
  shell <name> [-u user]     same, with the user's login shell
  nanny <name>               foreground respawn loop around 'nsm run'
  helper wg <conf>           (from a run script) bring up and hold WireGuard
  helper openvpn <ovpn> [-- openvpn args]
                             (from a run script) run openvpn with the fd trick
  version

flags:
  -C confdir   connection directories (default $NSM_CONFDIR or /etc/nsm)
  -v           print every ip/wg/nft command
  -n           dry run: print state changes instead of making them
  -pool CIDR   veth /31 pool (default 169.254.100.0/23)
  -wait DUR    how long 'run' waits for its substrate (default 30s)
`

// Version is set by the build (-ldflags "-X ...cli.Version=...").
var Version = "dev"

type globals struct {
	confdir string
	pool    netip.Prefix
	wait    time.Duration
	s       *sys.Sys
	out     io.Writer
	err     io.Writer
}

// Main runs nsm with args (without argv[0]) and returns the exit status.
func Main(args []string) int {
	g := &globals{out: os.Stdout, err: os.Stderr, s: sys.New()}
	fs := flag.NewFlagSet("nsm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	confdir := os.Getenv("NSM_CONFDIR")
	if confdir == "" {
		confdir = conn.DefaultConfDir
	}
	fs.StringVar(&g.confdir, "C", confdir, "")
	fs.BoolVar(&g.s.Verbose, "v", os.Getenv("NSM_VERBOSE") == "1", "")
	fs.BoolVar(&g.s.DryRun, "n", false, "")
	pool := fs.String("pool", conn.DefaultPool, "")
	fs.DurationVar(&g.wait, "wait", 30*time.Second, "")
	if err := fs.Parse(args); err != nil {
		return g.fail("%v\n%s", err, usage)
	}
	p, err := netip.ParsePrefix(*pool)
	if err != nil || !p.Addr().Is4() || p.Bits() > 30 {
		return g.fail("-pool %q: want an IPv4 prefix of /30 or wider", *pool)
	}
	g.pool = p.Masked()
	if fs.NArg() == 0 {
		fmt.Fprint(g.err, usage)
		return 2
	}
	cmd, rest := fs.Arg(0), fs.Args()[1:]
	switch cmd {
	case "run":
		return g.one(rest, g.cmdRun)
	case "up":
		return g.one(rest, g.cmdUp)
	case "down":
		return g.one(rest, g.cmdDown)
	case "nanny":
		return g.one(rest, g.cmdNanny)
	case "status":
		return g.cmdStatus(rest)
	case "exec":
		return g.cmdExec(rest, false)
	case "shell":
		return g.cmdExec(rest, true)
	case "helper":
		return g.cmdHelper(rest)
	case "version":
		fmt.Fprintln(g.out, "nsm", Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(g.out, usage)
		return 0
	}
	return g.fail("unknown command %q\n%s", cmd, usage)
}

func (g *globals) one(args []string, f func(name string) int) int {
	if len(args) != 1 {
		return g.fail("expected exactly one connection name\n%s", usage)
	}
	return f(args[0])
}

func (g *globals) fail(format string, a ...any) int {
	msg := fmt.Sprintf(format, a...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprint(g.err, "nsm: "+msg)
	return 1
}

// needRoot refuses to proceed unprivileged unless we are only planning.
func (g *globals) needRoot(what string) error {
	if os.Geteuid() != 0 && !g.s.DryRun {
		return fmt.Errorf("%s needs root (or -n to see the plan)", what)
	}
	return nil
}

// cmdHelper dispatches the transport helpers run scripts exec.
func (g *globals) cmdHelper(args []string) int {
	if len(args) == 0 {
		return g.fail("helper: want wg|openvpn|openvpn-up")
	}
	switch args[0] {
	case "wg":
		if len(args) != 2 {
			return g.fail("helper wg <conf>")
		}
		return g.helperWG(args[1])
	case "openvpn":
		if len(args) < 2 {
			return g.fail("helper openvpn <ovpn> [-- openvpn args]")
		}
		extra := args[2:]
		if len(extra) > 0 && extra[0] == "--" {
			extra = extra[1:]
		}
		return g.helperOpenVPN(args[1], extra)
	case "openvpn-up":
		return g.helperOpenVPNUp()
	}
	return g.fail("helper: unknown %q", args[0])
}
