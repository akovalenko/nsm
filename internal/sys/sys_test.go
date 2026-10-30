package sys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWhich(t *testing.T) {
	if p := which("/usr/sbin/ip"); p != "/usr/sbin/ip" {
		t.Errorf("path with a slash must pass through, got %q", p)
	}
	if p := which("./run"); p != "./run" {
		t.Errorf("relative path must pass through, got %q", p)
	}
	if p := which("ip"); !strings.HasPrefix(p, "/") {
		t.Errorf("ip should resolve to an absolute path, got %q", p)
	}

	dir := t.TempDir()
	tool := filepath.Join(dir, "nsmtesttool")
	if err := os.WriteFile(tool, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	old := toolSearchDirs
	toolSearchDirs = []string{dir}
	defer func() { toolSearchDirs = old }()

	t.Setenv("PATH", "") // the openvpn --up handler situation
	if p := which("nsmtesttool"); p != tool {
		t.Errorf("empty PATH: want %q, got %q", tool, p)
	}
	if p := which("definitely-not-a-tool-xyz"); p != "definitely-not-a-tool-xyz" {
		t.Errorf("unresolvable name must pass through, got %q", p)
	}
}
