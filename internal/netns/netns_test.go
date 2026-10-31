package netns

import (
	"strings"
	"testing"
)

func TestMainCheck(t *testing.T) {
	// Normally tests run where self == init (host or container main), and
	// MainCheck returns nil. If they somehow run inside a named namespace,
	// only the message shape is asserted.
	err := MainCheck()
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), "network namespace") {
		t.Errorf("unexpected message: %v", err)
	}
}

func TestIdentify(t *testing.T) {
	// Must not panic; in a named namespace it should find its own name.
	if got := Identify(); got != "" {
		t.Logf("running inside netns %q", got)
	}
}
