// Package helper holds the transport helpers a connection's run script may
// exec: `nsm helper wg` and `nsm helper openvpn`. They run inside the
// transport namespace (nsm run put them there) and learn the connection
// from the CONN_* environment nsm run exported.
package helper

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/akovalenko/nsm/internal/conn"
)

// RunDir is nsm's volatile state: pidfiles, logs, derived configs.
const RunDir = "/run/nsm"

// Env is what a helper knows about its connection.
type Env struct {
	Name  string // CONN_NAME
	Dir   string // CONN_DIR
	NS    string // CONN_NS: where the payload interface must end up
	Rides string // RIDES_NS: where we are running
	Meta  conn.Meta
}

// Vars renders the environment nsm run exports to the run script.
func Vars(c *conn.Conn) []string {
	return []string{
		"CONN_NAME=" + c.Name,
		"CONN_DIR=" + c.Dir,
		"CONN_NS=" + c.NS(),
		"RIDES_NS=" + c.Meta.Rides,
	}
}

// FromProcess reads CONN_* and reloads meta from CONN_DIR.
func FromProcess() (Env, error) {
	e := Env{
		Name:  os.Getenv("CONN_NAME"),
		Dir:   os.Getenv("CONN_DIR"),
		NS:    os.Getenv("CONN_NS"),
		Rides: os.Getenv("RIDES_NS"),
	}
	if e.Name == "" || e.Dir == "" || e.NS == "" {
		return e, fmt.Errorf("CONN_NAME/CONN_DIR/CONN_NS not set: helpers are meant to be exec'd from a run script under `nsm run`")
	}
	c, err := conn.Load(filepath.Dir(e.Dir), e.Name)
	if err != nil {
		return e, err
	}
	e.Meta = c.Meta
	return e, nil
}

// StateDir is the connection's directory under RunDir, created on demand.
func (e Env) StateDir() (string, error) {
	d := filepath.Join(RunDir, e.Name)
	return d, os.MkdirAll(d, 0o755)
}
