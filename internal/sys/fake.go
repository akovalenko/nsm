package sys

import (
	"errors"
	"strings"
	"sync"
)

// Fake is a scripted Sys for tests: it records every command line and
// answers from a table keyed by command prefix.
type Fake struct {
	*Sys
	mu    sync.Mutex
	Calls []string
	outs  map[string]string
	errs  map[string]error
}

// NewFake returns a Sys whose commands are recorded rather than run.
func NewFake() *Fake {
	f := &Fake{outs: map[string]string{}, errs: map[string]error{}}
	f.Sys = &Sys{Log: &strings.Builder{}, execFn: f.exec}
	return f
}

// On makes every command whose rendered line starts with prefix return out.
func (f *Fake) On(prefix, out string) { f.outs[prefix] = out }

// Fail makes every command whose rendered line starts with prefix fail.
func (f *Fake) Fail(prefix, msg string) { f.errs[prefix] = errors.New(msg) }

func (f *Fake) exec(stdin, name string, args ...string) (string, error) {
	line := Quote(append([]string{name}, args...))
	if stdin != "" {
		line += " <<< " + stdin
	}
	f.mu.Lock()
	f.Calls = append(f.Calls, line)
	f.mu.Unlock()
	// Longest matching prefix wins so specific answers can shadow general ones.
	best := ""
	for p := range f.errs {
		if strings.HasPrefix(line, p) && len(p) > len(best) {
			best = p
		}
	}
	bestOut := ""
	for p := range f.outs {
		if strings.HasPrefix(line, p) && len(p) > len(bestOut) {
			bestOut = p
		}
	}
	if best != "" && len(best) >= len(bestOut) {
		return "", &CmdError{Cmd: line, Stderr: f.errs[best].Error(), Err: f.errs[best]}
	}
	if bestOut != "" {
		return f.outs[bestOut], nil
	}
	return "", nil
}

// Called reports whether some recorded call starts with prefix.
func (f *Fake) Called(prefix string) bool {
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}
