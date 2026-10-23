// Package nanny is the built-in supervisor for systems without one: it
// respawns `nsm run <name>` with exponential backoff. Under systemd or
// runit/s6 it is not needed; they call `nsm run` themselves.
package nanny

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Options tune the respawn loop.
type Options struct {
	MinDelay time.Duration // first delay after a quick failure (default 1s)
	MaxDelay time.Duration // backoff ceiling (default 60s)
	Healthy  time.Duration // a run that lasted this long resets the backoff (default 10s)
	Log      io.Writer
}

func (o *Options) defaults() {
	if o.MinDelay == 0 {
		o.MinDelay = time.Second
	}
	if o.MaxDelay == 0 {
		o.MaxDelay = 60 * time.Second
	}
	if o.Healthy == 0 {
		o.Healthy = 10 * time.Second
	}
	if o.Log == nil {
		o.Log = os.Stderr
	}
}

// NextDelay implements the backoff policy: a run that lasted long enough
// resets the delay, a quick failure doubles it up to the ceiling.
func NextDelay(o Options, prev, ran time.Duration) time.Duration {
	o.defaults()
	if ran >= o.Healthy {
		return o.MinDelay
	}
	if prev < o.MinDelay {
		return o.MinDelay
	}
	d := prev * 2
	if d > o.MaxDelay {
		d = o.MaxDelay
	}
	return d
}

// Run supervises argv (the `nsm run <name>` command line) until SIGTERM or
// SIGINT, which it forwards to the child before exiting.
func Run(argv []string, o Options) error {
	o.defaults()
	logf := func(f string, a ...any) {
		fmt.Fprintf(o.Log, time.Now().Format("2006-01-02 15:04:05 ")+"nanny: "+f+"\n", a...)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)

	delay := time.Duration(0)
	for {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = nil
		cmd.Stdout = o.Log
		cmd.Stderr = o.Log
		start := time.Now()
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start %v: %w", argv, err)
		}
		logf("started pid %d", cmd.Process.Pid)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		stopping := false
	wait:
		for {
			select {
			case s := <-sig:
				stopping = true
				logf("got %v, stopping pid %d", s, cmd.Process.Pid)
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case err := <-done:
					logf("child exited: %v", exitStatus(err))
				case <-time.After(15 * time.Second):
					logf("child ignored SIGTERM, killing")
					_ = cmd.Process.Kill()
					<-done
				}
				return nil
			case err := <-done:
				ran := time.Since(start)
				delay = NextDelay(o, delay, ran)
				logf("child exited after %s: %s; respawn in %s", ran.Round(time.Second), exitStatus(err), delay)
				break wait
			}
		}
		if stopping {
			return nil
		}
		select {
		case <-sig:
			return nil
		case <-time.After(delay):
		}
	}
}

func exitStatus(err error) string {
	if err == nil {
		return "status 0"
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return "signal " + ws.Signal().String()
		}
		return "status " + strconv.Itoa(ee.ExitCode())
	}
	return err.Error()
}
