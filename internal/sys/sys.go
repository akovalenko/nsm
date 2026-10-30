// Package sys is the thin layer between nsm and the external tools it drives
// (ip, wg, nft, openvpn, getent). Every kernel-facing operation goes through
// one Sys value, so -v can print each command, -n can turn the whole program
// into a plan, and tests can script the tools without a kernel.
package sys

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Main names the initial network namespace: the one nsm itself and the
// supervisors run in. It is never addressed with `ip -n`.
const Main = "main"

// NetnsDir is where iproute2 keeps named namespaces.
const NetnsDir = "/run/netns"

// Sys runs external commands, optionally logging or only pretending.
type Sys struct {
	Verbose bool
	DryRun  bool
	Log     io.Writer

	// Which resolves bare tool names to executable paths; the default
	// searches PATH and the well-known system directories. Tests swap it
	// for the identity so recorded command lines stay machine-independent.
	Which func(name string) string

	// execFn is swapped in tests; nil means run for real.
	execFn func(stdin, name string, args ...string) (string, error)
}

// New returns a Sys that really runs commands.
func New() *Sys { return &Sys{Log: os.Stderr, Which: which} }

func (s *Sys) which(name string) string {
	if s.Which != nil {
		return s.Which(name)
	}
	return which(name)
}

// CmdError is returned when a tool exits non-zero; it carries stderr.
type CmdError struct {
	Cmd    string
	Stderr string
	Err    error
}

func (e *CmdError) Error() string {
	s := strings.TrimSpace(e.Stderr)
	if s == "" {
		return fmt.Sprintf("%s: %v", e.Cmd, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Cmd, s)
}

func (e *CmdError) Unwrap() error { return e.Err }

// Run executes a state-changing command and returns its stdout. In dry-run
// mode it only prints the plan line.
func (s *Sys) Run(name string, args ...string) (string, error) {
	return s.RunIn("", name, args...)
}

// RunIn is Run with a stdin payload (nft -f -, ...).
func (s *Sys) RunIn(stdin, name string, args ...string) (string, error) {
	return s.run(false, stdin, name, args...)
}

// Query executes a read-only command (ip ... show, getent, wg show). It runs
// even in dry-run mode, so the plan is computed against the real state, and
// is logged only in verbose mode.
func (s *Sys) Query(name string, args ...string) (string, error) {
	return s.run(true, "", name, args...)
}

func (s *Sys) run(query bool, stdin, name string, args ...string) (string, error) {
	line := Quote(append([]string{name}, args...))
	if s.Verbose || (s.DryRun && !query) {
		mark := "+"
		if query {
			mark = "?"
		}
		fmt.Fprintln(s.Log, mark, line)
		if stdin != "" && s.Verbose {
			for _, l := range strings.Split(strings.TrimRight(stdin, "\n"), "\n") {
				fmt.Fprintln(s.Log, "  |", l)
			}
		}
	}
	if s.execFn != nil {
		return s.execFn(stdin, name, args...)
	}
	if s.DryRun && !query {
		return "", nil
	}
	name = s.which(name)
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), &CmdError{Cmd: line, Stderr: errb.String(), Err: err}
	}
	return out.String(), nil
}

// Try is Run for best-effort cleanup: the error is logged in verbose mode and
// otherwise dropped.
func (s *Sys) Try(name string, args ...string) {
	if _, err := s.Run(name, args...); err != nil && s.Verbose {
		fmt.Fprintln(s.Log, "  (ignored)", err)
	}
}

// IP runs `ip` inside namespace ns ("main" or "" = the current one).
func (s *Sys) IP(ns string, args ...string) (string, error) {
	return s.Run("ip", nsArgs(ns, args)...)
}

// TryIP is Try for ip.
func (s *Sys) TryIP(ns string, args ...string) { s.Try("ip", nsArgs(ns, args)...) }

// IPQ is Query for ip.
func (s *Sys) IPQ(ns string, args ...string) (string, error) {
	return s.Query("ip", nsArgs(ns, args)...)
}

// InQ is Query for a command inside ns.
func (s *Sys) InQ(ns string, name string, args ...string) (string, error) {
	if ns == "" || ns == Main {
		return s.Query(name, args...)
	}
	return s.Query("ip", append([]string{"netns", "exec", ns, name}, args...)...)
}

func nsArgs(ns string, args []string) []string {
	if ns == "" || ns == Main {
		return args
	}
	return append([]string{"-n", ns}, args...)
}

// In runs an arbitrary command inside ns via `ip netns exec`, which also
// gives it that namespace's /etc/netns/<ns>/resolv.conf.
func (s *Sys) In(ns string, name string, args ...string) (string, error) {
	return s.InWithStdin("", ns, name, args...)
}

// InWithStdin is In with a stdin payload.
func (s *Sys) InWithStdin(stdin, ns string, name string, args ...string) (string, error) {
	if ns == "" || ns == Main {
		return s.RunIn(stdin, name, args...)
	}
	// The inner command is executed by ip with our environment, which in an
	// openvpn --up handler has no PATH — resolve it to an absolute path.
	return s.RunIn(stdin, "ip", append([]string{"netns", "exec", ns, s.which(name)}, args...)...)
}

// Command builds a foreground exec.Cmd running inside ns with inherited
// stdio; the caller starts and waits for it. In dry-run mode it is replaced
// by `true` after the plan line is printed.
func (s *Sys) Command(ns string, name string, args ...string) *exec.Cmd {
	full := append([]string{name}, args...)
	if ns != "" && ns != Main {
		full = append([]string{"ip", "netns", "exec", ns}, full...)
	}
	if s.Verbose || s.DryRun {
		fmt.Fprintln(s.Log, "+", Quote(full), "&")
	}
	if s.DryRun {
		full = []string{"true"}
	} else {
		full[0] = s.which(full[0])
	}
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// toolSearchDirs is where bare tool names are looked up beyond PATH: nsm
// runs in environments without one (an openvpn --up handler has no PATH at
// all, systemd units may have a minimal one).
var toolSearchDirs = []string{
	"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin",
}

// which resolves a bare tool name to an absolute path: PATH first (when it
// yields anything), then the well-known system directories. Names that
// already contain a slash are returned unchanged; an unresolvable name is
// returned as is and fails at exec time with the usual error.
func which(name string) string {
	if strings.ContainsRune(name, '/') {
		return name
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range toolSearchDirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return name
}

// Quote renders argv for humans in the plan/verbose output.
func Quote(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if a == "" || strings.ContainsAny(a, " \t\n\"'$`\\!*?[]{}|&;<>()#") {
			b.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

// IsExit reports whether err is a command failure (as opposed to a missing
// binary or an internal error).
func IsExit(err error) bool {
	var ce *CmdError
	if !errors.As(err, &ce) {
		return false
	}
	var ee *exec.ExitError
	return errors.As(ce.Err, &ee)
}
