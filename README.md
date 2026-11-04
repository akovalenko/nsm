# nsm

A netns-first connection manager for Linux. Every tunnel (WireGuard, OpenVPN,
anything a script can bring up) lives in **its own network namespace**; the
tunnel's transport socket is born in an **explicitly named namespace**
(`rides=`), so "this VPN goes over wifi" or "this VPN goes through that other
VPN" is a one-word construction instead of a policy-routing puzzle; and the
prefixes the main namespace should reach are **mounted** one by one, with
collisions refused out loud instead of settled by route metrics.

Connection configs stay in their **native formats** (`wg.conf`, `.ovpn`). nsm
owns orchestration only: namespaces, veths, routes into main, DNS plumbing,
supervision. It has no daemon and no database; the kernel is the state and
`nsm status` recomputes everything from it.

It is init-agnostic. The one contract is a foreground `nsm run <name>` that a
supervisor restarts: systemd, runit/s6, or the built-in nanny.

## Connection directory

```
/etc/nsm/<name>/            (or -C <confdir>, or $NSM_CONFDIR)
  run        mandatory, executable: starts the transport in the foreground
  meta       optional key=value stanza (see below)
  conf/      native artifacts: wg.conf, client.ovpn, certs... nsm never reads them
```

`<name>` is 1–12 chars of `[A-Za-z0-9_.-]` (it also names the netns and the
`ve-<name>` veth). `meta`:

```sh
# netns that carries the transport socket: "main" (default) or another connection
rides=lappy
# IPv4 prefixes to mount into main: space- or comma-separated CIDRs
# (a bare address means /32); multiple expose= lines accumulate,
# duplicates collapse
expose="10.48.0.0/16 10.10.0.0/24 10.48.1.53"
# resolv.conf for this connection's netns: literal directives and/or file
# paths (absolute, or relative to the connection dir when they contain "/");
# multiple resolv= lines accumulate in order, duplicates collapse
resolv="nameserver 1.1.1.1"
resolv="options edns0 trust-ad"
# payload interface MTU override
mtu=1370
```

### The `run` contract

`nsm run <name>` execs `<dir>/run` **inside the transport namespace**
(`rides`), with cwd = the connection directory and these variables:

| var        | value                                          |
|------------|------------------------------------------------|
| `CONN_NAME`| the connection name                            |
| `CONN_DIR` | its directory                                  |
| `CONN_NS`  | the netns the payload interface must end up in |
| `RIDES_NS` | the netns `run` is executing in                |

`run` must bring the transport up, put its interface into `$CONN_NS`, and
stay in the foreground; when it exits, the connection is down and the
supervisor restarts it. Two helpers cover the common cases:

```sh
#!/bin/sh
exec nsm helper wg conf/wg.conf
```

```sh
#!/bin/sh
exec nsm helper openvpn conf/client59.ovpn
```

**`nsm helper wg`** accepts a wg-quick style config. It strips the wg-quick
keys (`Address`, `DNS`, `MTU`, `Table`, hooks), resolves `Endpoint` hostnames
*here*, in the transport namespace (with its resolver; the connection ns has
no route out yet), creates the interface here so its UDP socket is pinned to
this namespace, moves it into `$CONN_NS`, applies `wg setconf`, addresses,
MTU and `default dev <if>`, then **holds**: it stays alive until told to stop
or until the interface disappears. WireGuard has no daemon; the holder gives
it one, so it is supervised like everything else.

**`nsm helper openvpn`** runs openvpn with `--ifconfig-noexec --route-noexec`
and an `--up` handler (`nsm helper openvpn-up`). openvpn keeps its transport
socket and its tun **file descriptor** here; the handler moves the tun
*device* into `$CONN_NS` (a tun fd follows the device across namespaces) and
configures address, MTU, `default`, pushed routes and pushed DNS there. The
handler is idempotent, so `--up-restart`/`--persist-tun` reconnects are fine.

Anything else is a `run` script of your own: it only has to end with the
interface in `$CONN_NS` and a foreground process.

## Commands

```
nsm run <name>       foreground; what a supervisor should run
nsm up <name>        detach the built-in nanny (respawn with backoff)
nsm down <name>      stop + tear down (veth, routes, netns, resolv.conf, state)
nsm status [name..]  recomputed from the kernel; lists orphans too
nsm exec <name> [-u user] cmd...
nsm shell <name> [-u user]
nsm nanny <name>     the respawn loop itself, in the foreground
```

Global flags: `-C confdir`, `-v` (print every ip/wg/nft command), `-n` (dry
run: print state changes instead of making them; read-only probes still run,
so the plan matches the real state), `-pool CIDR` (veth /31 pool, default
`169.254.100.0/23`), `-wait DUR` (substrate wait, default 30s), `-any-ns`
(run inside a non-init network namespace, treating it as main).

`run`/`up`/`nanny`/`down` refuse to start unless the current network
namespace is init's (PID 1): "main" is wherever nsm itself sits, so an
accidental `ip netns exec X nsm run ...` would build every main-side object
(veth, mount routes, collision checks, NAT) inside X — and `ip netns add`
from that context registers half-visible names. `status` only warns; `nsm
exec` is not checked (setns by name works from anywhere). `-any-ns` opts out
for deployments where the current namespace is the intended main.

### What `run` does before the transport

1. Waits (up to `-wait`) for the substrate: the `rides` namespace exists and
   has a default route. Not there yet → keep waiting; timeout → exit non-zero
   and let the supervisor try again. **Start order never matters.**
2. Ensures netns `<name>` with `lo` up, and `/etc/netns/<name>/resolv.conf`
   from `resolv=`.
3. Applies `expose=` (below).
4. Starts `run` in the `rides` namespace; then watches the substrate every
   5s and **kills the transport and exits** when the substrate namespace is
   gone, was recreated, or lost its default route. The supervisor restarts,
   step 1 waits again. This replaces a dependency graph with crash-only
   convergence.

### Mounts (`expose=`)

For each exposed prefix, main gets `route <P> via <ns-veth-addr> dev
ve-<name>`. Plumbing: a veth pair (`ve-<name>` in main, `ve0` in the ns) with
a /31 from the pool, forwarding in the ns, and **double NAT**: main
masquerades into the veth address on its way into `ve-<name>`, the ns
masquerades the veth pool into the tunnel address. Neither side has to know
the other's addressing, and return traffic needs no policy routing.

**Collision rule:** a prefix that overlaps any existing non-default route in
main (a connected LAN, another connection's mount, a host route) is
**refused** — the route is not added, the reason is printed by `run` and
shown by `status`, the other prefixes and the connection itself proceed.
Mounts left over from a previous `expose=` value are removed. With an empty
`expose=` the veth is torn down.

Example: the office pushes `192.168.1.0/24` and home wifi is `192.168.1.0/24`
too. Inside the connection's ns the office meaning is intact
(`nsm exec office ssh 192.168.1.10`); mounting it into main is refused with
`overlaps 192.168.1.0/24 dev wlan0`. Both meanings exist, neither wins by
accident.

If the transport is down, mounted prefixes lead into a namespace with no
default route: traffic fails closed instead of leaking out the physical
uplink. Kill switch by construction.

### `nsm exec` / `nsm shell`

`sudo nsm exec office firefox --no-remote -P office`: setns into the
connection's netns, a private mount namespace with that connection's
`resolv.conf` bound over `/etc/resolv.conf`, then drop to the invoking user
(`$SUDO_UID`, or `-u user`) and exec. Beyond HOME/USER it restores what
sudo's env reset tends to lose if the files are there: `XDG_RUNTIME_DIR`,
`WAYLAND_DISPLAY`, `DBUS_SESSION_BUS_ADDRESS`.

The same mount namespace shadows the resolver-daemon sockets (`nscd`,
`systemd-resolved`, `sssd`) with a `/dev/null` bind. Filesystem unix sockets
cross netns boundaries, and without the shadow a daemon in main keeps
answering glibc's lookups — with main's resolver, and negative answers
included — so the connection's resolv.conf would never be consulted; per-ns
`nsswitch.conf` cannot prevent this, because the nscd interception happens
before nsswitch.

Namespace facts to know: abstract unix sockets are per-netns (X11 falls back
to its filesystem socket and works; a D-Bus `unix:abstract=` address does
not), `127.0.0.1` is per-netns (local CUPS, proxies and dev servers are not
reachable from inside), and browsers hand new windows to an already-running
instance unless told otherwise (`--no-remote`, separate profile).

## Supervision

Pick one:

- **systemd**: see `examples/nsm@.service` (`ExecStart=nsm run %i`,
  `Restart=always`). `systemctl enable nsm@lappy nsm@client59`.
- **runit/s6**: `exec nsm run <name>` as the service's run script.
- **nothing**: `nsm up <name>` detaches a nanny with exponential backoff
  (1s doubling to 60s, reset after a run that lasted 10s), logging to
  `/run/nsm/<name>/log`. `nsm down <name>` stops it.

`nsm down` works in every mode; under an external supervisor stop the
service first or it will resurrect the connection.

## Worked example

```
/etc/nsm/lappy/     run: exec nsm helper wg conf/wg.conf
                    (no meta: rides=main)
/etc/nsm/client59/  run: exec nsm helper openvpn conf/client59.ovpn
                    meta: rides=lappy
                          expose="10.48.0.0/16 10.10.0.0/24"
```

- `lappy` rides wifi; its wg socket lives in main and roams with it.
- `client59` rides lappy: its openvpn process (and the resolution of
  `remote`) lives in lappy's namespace; the only way out is the WireGuard
  tunnel. When lappy dies, `nsm run client59` notices, dies, and waits for
  lappy to come back.
- Office prefixes are reachable from main through the mounts; the office
  default route exists only inside `client59`'s namespace.

## Requirements

Linux, root, iproute2 (`ip`), `nft`, `sysctl`, `getent`; `wg` and/or
`openvpn` for the helpers. IPv4 for the mounts and default routes in this
version.

## Status

v0: run/up/down/status/exec, wg and openvpn helpers, mounts with collision
refusal, nanny. Planned: orphan cleanup, `expose=auto`, IPv6, and an
unprivileged `nsm exec` (connection namespaces owned by a per-user user
namespace).

## License

Public domain (see `UNLICENSE`).
