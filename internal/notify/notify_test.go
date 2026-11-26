package notify

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// listenUnixgram opens the manager's end at addr ("@name" is the abstract
// form systemd hands out) and returns it with a one-datagram reader.
func listenUnixgram(t *testing.T, addr string) *net.UnixConn {
	t.Helper()
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func recv(t *testing.T, c *net.UnixConn) string {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:n])
}

func TestReady(t *testing.T) {
	for _, tc := range []struct{ name, addr string }{
		{"path", filepath.Join(t.TempDir(), "notify.sock")},
		{"abstract", "@nsm-notify-test-" + strconv.Itoa(os.Getpid())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := listenUnixgram(t, tc.addr)
			t.Setenv("NOTIFY_SOCKET", tc.addr)
			if sent, err := Ready(); err != nil || !sent {
				t.Fatalf("Ready() = %t, %v; want true, nil", sent, err)
			}
			if got := recv(t, c); got != "READY=1" {
				t.Errorf("datagram %q, want %q", got, "READY=1")
			}
			if _, set := os.LookupEnv("NOTIFY_SOCKET"); set {
				t.Error("NOTIFY_SOCKET still in the environment")
			}
		})
	}
}

func TestReadyNoSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if sent, err := Ready(); err != nil || sent {
		t.Fatalf("Ready() = %t, %v; want false, nil", sent, err)
	}
	// A stale address is not "no manager": the caller learns the signal
	// was lost instead of the start job running into its timeout.
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	if sent, err := Ready(); err == nil || sent {
		t.Fatalf("Ready() to a missing socket = %t, %v; want false, error", sent, err)
	}
}
