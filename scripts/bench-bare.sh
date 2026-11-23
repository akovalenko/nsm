#!/bin/sh
# bench-bare.sh -- a rootless lab for examples/bare: a namespace whose
# uplink is main itself, while main's own traffic rides a WireGuard tunnel
# set up the way NetworkManager sets it up (auto-default-route: the default
# route in a private table, two rules selecting it, main's table keeping
# the physical uplink).
#
# Needs no root: everything runs inside one throwaway user+mount+net+pid
# namespace (unshare -Ur -m -n -p). The host's namespaces, routes, rules and
# nft tables are not touched -- /run/netns, /etc/netns and /run/nsm are
# covered with private tmpfs, and the pid namespace takes every process down
# with the bench. NetworkManager itself is not run: its kernel-side result
# is reproduced by hand (table, fwmark, the two rules), copied from a live
# NM 1.46 host. Needs /run/netns and /etc/netns to exist.
#
# usage: scripts/bench-bare.sh [path-to-nsm] [examples-dir]
#        (defaults: ./nsm and ./examples next to scripts/). As root,
#        NSM_BENCH_ROOT=1 skips the user namespace, and what a rootless one
#        cannot do -- setgroups, hence nsm enter and exec -u -- is covered.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
NSM=${1:-$(dirname "$HERE")/nsm}
EXAMPLES=${2:-$(dirname "$HERE")/examples}
[ -x "$NSM" ] || { echo "usage: $0 [path-to-nsm] [examples-dir]" >&2; exit 2; }
[ -f "$EXAMPLES/bare/run" ] || { echo "$EXAMPLES/bare/run not found" >&2; exit 2; }
command -v unshare ip wg nft nc python3 sysctl pgrep ping >/dev/null || {
    echo "need: unshare ip wg nft nc python3 sysctl pgrep ping" >&2; exit 2; }
NSM=$(cd "$(dirname "$NSM")" && pwd)/$(basename "$NSM")
EXAMPLES=$(cd "$EXAMPLES" && pwd)

set -- --mount --net --pid --fork --mount-proc
[ "${NSM_BENCH_ROOT:-}" = 1 ] || set -- --user --map-root-user "$@"

# The lab script rides fd 3, so children inheriting stdin (nc, python)
# cannot eat it.
exec unshare "$@" sh -c 'exec sh /dev/fd/3 "$@"' sh "$NSM" "$EXAMPLES" 3<<'INNER'
# ---- root inside the lab; "main" is this namespace, PID 1 is this shell ----
NSM=$1
EXAMPLES=$2
pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok    $*"; }
bad() { fail=$((fail + 1)); echo "  FAIL  $*"; }
check() { # LABEL WANT GOT
    if [ "$2" = "$3" ]; then ok "$1: $3"; else bad "$1: want [$2], got [$3]"; fi
}
step() { echo; echo "== $*"; }
in_bare() { "$NSM" -C "$LAB/etc" exec bare -- "$@"; }
wait_bare() { # until the veth end in the namespace carries an address again
    i=0
    while [ $i -lt 80 ]; do
        if ip -n bare -4 addr show dev eth0 2>/dev/null | grep -q 'inet ' &&
               ip -n bare link show eth0 up 2>/dev/null | grep -q UP; then
            sleep 0.5; return 0
        fi
        sleep 0.25; i=$((i + 1))
    done
    return 1
}

LAB=$(mktemp -d /tmp/nsm-bare.XXXXXX)
trap 'rm -rf "$LAB"' EXIT
umask 077
KEYS=$LAB/keys; mkdir -m 700 "$KEYS"

[ -d /run/netns ] && [ -d /etc/netns ] || {
    echo "need /run/netns and /etc/netns to exist" >&2; exit 2; }
mount -t tmpfs lab /run/netns
mount -t tmpfs lab /etc/netns
if [ -d /run/nsm ]; then
    mount -t tmpfs lab /run/nsm
else
    mount -t tmpfs lab /run
    mkdir /run/netns
    mount -t tmpfs lab /run/netns
fi
ip link set lo up
sysctl -q -w net.ipv4.ip_forward=1

# The world: LAN 203.0.113.0/24 (main's) and an "internet" segment
# 198.51.100.0/24 where the tunnel server lives. `inet` is both main's
# gateway and the host every probe talks to: it answers with the peer
# address it sees, which tells the paths apart -- the tunnel server NATs to
# 198.51.100.2, main's own LAN address is 203.0.113.2.
step "topology"
ip netns add inet
ip netns add srv
ip link add wan0 type veth peer name lan0 netns inet
ip addr add 203.0.113.2/24 dev wan0
ip link set wan0 up
ip -n inet addr add 203.0.113.1/24 dev lan0
ip -n inet link set lan0 up
ip -n inet link add net0 type veth peer name eth0 netns srv
ip -n inet addr add 198.51.100.1/24 dev net0
ip -n inet link set net0 up
ip -n inet link set lo up
ip netns exec inet sysctl -q -w net.ipv4.ip_forward=1
ip -n srv addr add 198.51.100.2/24 dev eth0
ip -n srv link set eth0 up
ip -n srv link set lo up
ip -n srv route add default via 198.51.100.1
ip netns exec srv sysctl -q -w net.ipv4.ip_forward=1
# What NM leaves in main's table: the physical uplink.
ip -4 route add default via 203.0.113.1 dev wan0 metric 600

ip netns exec inet python3 -u -c '
import socket, threading
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 9000)); s.listen(64)
def h(c, a):
    c.sendall(("peer=%s\n" % a[0]).encode()); c.close()
while True:
    c, a = s.accept(); threading.Thread(target=h, args=(c, a), daemon=True).start()
' &
echo "  main=203.0.113.2  gateway+internet=203.0.113.1/198.51.100.1  tunnel server=198.51.100.2"

# The tunnel, as NetworkManager 1.46 leaves a 0/0 WireGuard profile with
# auto-default-route on: the default route in table = fwmark and the pair of
# rules; 30000 is the host exemption the real profile carries (its second
# one, a uidrange, is written over netlink and iproute2 refuses to spell it).
step "tunnel: lappy as NetworkManager sets it up"
wg genkey > "$KEYS/srv"; wg pubkey < "$KEYS/srv" > "$KEYS/srv.pub"
wg genkey > "$KEYS/cli"; wg pubkey < "$KEYS/cli" > "$KEYS/cli.pub"
ip -n srv link add wg0 type wireguard
ip -n srv addr add 10.7.0.1/24 dev wg0
ip netns exec srv wg set wg0 private-key "$KEYS/srv" listen-port 51820
ip netns exec srv wg set wg0 peer "$(cat "$KEYS/cli.pub")" allowed-ips 10.7.0.2/32
ip -n srv link set wg0 up
ip netns exec srv nft -f - <<'EOF'
table ip nat {
	chain post {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "eth0" masquerade
	}
}
EOF
ip link add lappy type wireguard
wg set lappy private-key "$KEYS/cli" fwmark 52166
wg set lappy peer "$(cat "$KEYS/srv.pub")" endpoint 198.51.100.2:51820 \
    allowed-ips 0.0.0.0/0 persistent-keepalive 25
ip addr add 10.7.0.2/32 dev lappy
ip link set lappy up
ip -4 route add default dev lappy table 52166 metric 20053
# The endpoint keeps a host route of its own, so that "uplink gone" below
# means exactly that -- no default route in main -- while the tunnel lives.
ip -4 route add 198.51.100.2/32 via 203.0.113.1 dev wan0
ip -4 rule add pref 30000 to 87.249.36.55 lookup main
ip -4 rule add pref 31006 lookup main suppress_prefixlength 0
ip -4 rule add pref 31007 not fwmark 52166 lookup 52166
ip -4 rule | sed 's/^/        /'

probe() { nc -w 3 "$1" 9000 2>/dev/null; }
step "main rides the tunnel, LAN still reachable (the NM layout itself)"
r=$(ip -4 route get 198.51.100.1 | head -1)
case $r in *"dev lappy table 52166"*) ok "route get internet: $r";; *) bad "route get internet: $r";; esac
check "main -> internet seen as" peer=198.51.100.2 "$(probe 198.51.100.1)"
check "main -> LAN address stays on the LAN" peer=203.0.113.2 "$(probe 203.0.113.1)"
check "lappy handshake" yes \
    "$([ "$(wg show lappy latest-handshakes | awk '{print $2}')" -gt 0 ] && echo yes || echo no)"
check "main -> LAN gateway ping" ok "$(ping -c1 -W2 203.0.113.1 >/dev/null 2>&1 && echo ok || echo FAIL)"

# The connection under test.
step "nsm up bare (rule owned by the run script)"
mkdir -p "$LAB/etc/bare" "$LAB/etc/m1"
cp "$EXAMPLES/bare/run" "$EXAMPLES/bare/meta" "$LAB/etc/bare/"
chmod +x "$LAB/etc/bare/run"
# A second connection with a mount, to see the two kinds coexist. Its
# "transport" is just the address main mounts, owned by the namespace.
printf '%s\n' '#!/bin/sh' \
    'ip -n m1 link add payload0 type dummy 2>/dev/null || :' \
    'ip -n m1 addr replace 10.48.0.1/32 dev payload0' \
    'ip -n m1 link set payload0 up' \
    'exec sleep 1000000' > "$LAB/etc/m1/run"
chmod +x "$LAB/etc/m1/run"
echo 'expose=10.48.0.1/32' > "$LAB/etc/m1/meta"
"$NSM" -C "$LAB/etc" up bare
wait_bare || bad "bare did not come up"
"$NSM" -C "$LAB/etc" status bare | sed 's/^/        /'
check "lookup rule in main" 1 "$(ip -4 rule | grep -c '^30200:.*iif ve-bare lookup main')"
check "companion rule in main" 1 "$(ip -4 rule | grep -c '^30201:.*iif ve-bare unreachable')"
check "bare -> internet seen as (THE point)" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
check "bare -> LAN gateway seen as" peer=203.0.113.2 "$(in_bare nc -w 3 203.0.113.1 9000)"
check "bare -> LAN gateway ping" ok "$(in_bare ping -c1 -W2 203.0.113.1 >/dev/null 2>&1 && echo ok || echo FAIL)"
check "bare resolv.conf" "nameserver 1.1.1.1" "$(in_bare sh -c 'head -1 /etc/resolv.conf')"
check "main -> internet still seen as" peer=198.51.100.2 "$(probe 198.51.100.1)"
cp /run/nsm/bare/log "$LAB/log.bare"

step "sudo-facing entry: nsm enter (target pinned to \$SUDO_UID)"
r=$(SUDO_UID=0 "$NSM" -C "$LAB/etc" enter bare nc -w 3 198.51.100.1 9000 2>&1)
case $r in
peer=*) check "enter bare -> internet seen as" peer=203.0.113.2 "$r";;
*) echo "  note: enter drops with setgroups, which a rootless user namespace"
   echo "        has denied for good ($r); its network path is exec's, covered above";;
esac

step "control: without the rules bare rides the tunnel"
ip -4 rule del pref 30200; ip -4 rule del pref 30201
check "no rules -> seen as" peer=198.51.100.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
ip -4 rule add pref 30200 iif ve-bare lookup main
check "lookup rule alone -> seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
ip -4 rule add pref 30201 iif ve-bare unreachable
check "both rules -> seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"

step "uplink gone (no default route in main), tunnel still up"
ip -4 route del default via 203.0.113.1 dev wan0 metric 600
check "main -> internet still seen as" peer=198.51.100.2 "$(probe 198.51.100.1)"
ip -4 rule del pref 30201
check "lookup rule alone -> falls through into the tunnel" peer=198.51.100.2 \
    "$(in_bare nc -w 3 198.51.100.1 9000)"
ip -4 rule add pref 30201 iif ve-bare unreachable
check "with the companion -> no answer at all (fails closed)" "" "$(in_bare nc -w 2 198.51.100.1 9000)"
ip -4 route add default via 203.0.113.1 dev wan0 metric 600
check "uplink back, bare -> internet seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"

step "strict rp_filter in main"
sysctl -q -w net.ipv4.conf.all.rp_filter=1
check "bare -> internet seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
sysctl -q -w net.ipv4.conf.all.rp_filter=2

step "nsm mounts coexist (m1 exposes 10.48.0.1/32)"
"$NSM" -C "$LAB/etc" up m1
i=0
while [ $i -lt 40 ] && ! ip -4 route show 10.48.0.1 2>/dev/null | grep -q .; do
    sleep 0.25; i=$((i + 1))
done
r=$(ip -4 route get 10.48.0.1 | head -1)
case $r in *"via 169.254.100.1 dev ve-m1"*) ok "main route get mount: $r";; *) bad "main route get mount: $r";; esac
check "main -> mounted address ping" ok "$(ping -c1 -W2 10.48.0.1 >/dev/null 2>&1 && echo ok || echo FAIL)"
check "bare -> mounted address ping (bare follows main's table)" ok \
    "$(in_bare ping -c1 -W2 10.48.0.1 >/dev/null 2>&1 && echo ok || echo FAIL)"
"$NSM" -C "$LAB/etc" status | sed 's/^/        /'
ip -4 rule | sed 's/^/        /'
nft list table ip nsm | sed 's/^/        /'

step "restart: run script killed with SIGKILL, the nanny rebuilds"
pkill -9 -f "$LAB/etc/bare/run"
i=0; while [ $i -lt 20 ] && pgrep -f "$LAB/etc/bare/run" >/dev/null; do
    sleep 0.25; i=$((i + 1))
done
i=0; while [ $i -lt 80 ] && ! pgrep -f "$LAB/etc/bare/run" >/dev/null; do
    sleep 0.25; i=$((i + 1))
done
wait_bare || bad "bare did not come back"
check "rule count after respawn" 2 "$(ip -4 rule | grep -c '^3020[01]:')"
check "masquerade rules after respawn" 1 "$(nft list chain ip nsm up-bare | grep -c masquerade)"
check "bare -> internet seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
check "status after respawn" running \
    "$("$NSM" -C "$LAB/etc" status bare | sed -n 's/^  transport: \(running\).*/\1/p')"

step "nsm down bare"
"$NSM" -C "$LAB/etc" down bare
check "veth gone" 0 "$(ip -4 link show ve-bare 2>/dev/null | wc -l)"
check "rules gone" 0 "$(ip -4 rule | grep -c '^3020[01]:')"
check "chain gone" 0 "$(nft list table ip nsm 2>/dev/null | grep -c 'up-bare')"
check "netns gone" 0 "$(ip netns list | grep -c '^bare')"
"$NSM" -C "$LAB/etc" down m1
echo "  table ip nsm after both are down: $(nft list table ip nsm 2>/dev/null | tr -s '\n' ' ')"
check "no chain left in table ip nsm" 0 "$(nft list table ip nsm 2>/dev/null | grep -c 'chain ')"

step "variant: the rule owned by the tunnel profile (NM ipv4.routing-rules)"
ip -4 rule add pref 30200 from 169.254.53.1 lookup main
ip -4 rule add pref 30201 from 169.254.53.1 unreachable
sed 's/^RULE_PREF=30200$/RULE_PREF=/' "$EXAMPLES/bare/run" > "$LAB/etc/bare/run"
chmod +x "$LAB/etc/bare/run"
"$NSM" -C "$LAB/etc" up bare
wait_bare || bad "bare did not come up"
check "no rule from the script" 0 "$(ip -4 rule | grep -c 'iif ve-bare')"
check "bare -> internet seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
ip -4 route del default via 203.0.113.1 dev wan0 metric 600
check "uplink gone -> no answer at all" "" "$(in_bare nc -w 2 198.51.100.1 9000)"
ip -4 route add default via 203.0.113.1 dev wan0 metric 600
"$NSM" -C "$LAB/etc" down bare
check "the profile's rules outlive down (NM's to keep)" 2 "$(ip -4 rule | grep -c '^3020[01]:')"
ip -4 rule del pref 30200; ip -4 rule del pref 30201
cp "$EXAMPLES/bare/run" "$LAB/etc/bare/run"

step "everything SIGKILLed, then down and up again"
"$NSM" -C "$LAB/etc" up bare
wait_bare || bad "bare did not come up"
pkill -9 -f "nsm -C $LAB/etc"
pkill -9 -f "$LAB/etc/bare/run"
sleep 0.5
"$NSM" -C "$LAB/etc" down bare
echo "  after down from a killed run: rules=$(ip -4 rule | grep -c '^3020[01]:') chains=$(nft list table ip nsm 2>/dev/null | grep -c 'up-bare')"
"$NSM" -C "$LAB/etc" up bare
wait_bare || bad "bare did not come up"
check "rule count after up" 2 "$(ip -4 rule | grep -c '^3020[01]:')"
check "masquerade rules after up" 1 "$(nft list chain ip nsm up-bare | grep -c masquerade)"
check "bare -> internet seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
"$NSM" -C "$LAB/etc" down bare

step "kernel: does an iif rule follow its device?"
ip link add t0 type veth peer name t1
ip -4 rule add pref 30500 iif t0 lookup main
ip link del t0
if ip -4 rule | grep -q '^30500:'; then
    echo "  note: the rule outlives its device: $(ip -4 rule | grep '^30500:')"
    ip link add t0 type veth peer name t1
    if ip -4 rule del pref 30500 iif t0 lookup main 2>/dev/null; then
        echo "  note: ...and follows the recreated device by name"
    else
        echo "  note: ...bound to the old device index: 'del ... iif t0' does not match it"
    fi
else
    echo "  note: the rule dies with its device"
fi
ip -4 rule del pref 30500 2>/dev/null || :
ip link del t0 2>/dev/null || :

echo
if [ "$fail" != 0 ] && [ -f "$LAB/log.bare" ]; then
    echo "--- nsm log (bare, first run)"; sed 's/^/  | /' "$LAB/log.bare"
fi
echo "passed $pass, failed $fail"
[ "$fail" = 0 ]
INNER
