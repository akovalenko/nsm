// Command nsm is a netns-first connection manager: every tunnel lives in its
// own network namespace, its transport socket is born in an explicitly named
// namespace (`rides`), and the prefixes main should reach are mounted
// explicitly, with collisions refused out loud. Connection configs stay in
// their native formats (wg.conf, .ovpn); nsm only orchestrates.
//
// The design lives in the author's notes (plans/netns-network-manager-spec.md);
// README.md is the operational contract.
package main

import (
	"os"

	"github.com/akovalenko/nsm/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
