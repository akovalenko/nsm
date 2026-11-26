// Package notify speaks the readiness protocol of sd_notify(3) from
// scratch: one datagram to $NOTIFY_SOCKET, no libsystemd.
package notify

import (
	"net"
	"os"
)

// Ready tells the service manager that start-up is finished (READY=1).
// Only a Type=notify unit waits for it: without $NOTIFY_SOCKET -- no
// manager, or a unit of another type -- nothing is sent and Ready reports
// false. The variable is dropped from the environment either way, since the
// processes nsm starts later are not the one the manager takes notifications
// from (NotifyAccess=main).
func Ready() (bool, error) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if err := os.Unsetenv("NOTIFY_SOCKET"); err != nil {
		return false, err
	}
	if socket == "" {
		return false, nil
	}
	// The manager names its socket in the environment: an abstract one
	// ("@...") or a path. The net package takes both forms as is.
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return false, err
	}
	return true, nil
}
