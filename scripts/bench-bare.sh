#!/bin/sh
# bench-bare.sh -- a rootless lab for examples/bare: a transport-less
# connection whose namespace goes out by main's physical uplink while main
# itself rides a WireGuard tunnel set up the way NetworkManager sets it up
# (auto-default-route: the default route in a private table, two rules
# selecting it, main's table keeping the physical uplink).
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
[ -f "$EXAMPLES/bare/meta" ] || { echo "$EXAMPLES/bare/meta not found" >&2; exit 2; }
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
nsm() { "$NSM" -C "$LAB/etc" "$@"; }
count() { # how many policy rules name the veth
    ip -j -4 rule show | python3 -c 'import json,sys; print(sum(1 for r in json.load(sys.stdin) if r.get("iif") == "ve-bare"))'
}
wait_bare() { # until the default route, the NAT chain and -- with rule= -- the pair are there
    i=0
    while [ $i -lt 80 ]; do
        if ip -n bare -j -4 route show default 2>/dev/null | grep -q '"dev":"ve0"' &&
               nft list chain ip nsm ve-bare 2>/dev/null | grep -q masquerade &&
               [ "$(count)" = "$want_rules" ]; then
            sleep 0.5; return 0
        fi
        sleep 0.25; i=$((i + 1))
    done
    return 1
}
# meta_bare [RULE] [WANT_RULES] -- rewrite bare's meta and bring it up
meta_bare() {
    nsm down bare >/dev/null 2>&1 || :
    {
        echo "uplink=main"
        echo 'resolv="nameserver 9.9.9.9"'
        [ -n "${1-}" ] && echo "rule=$1"
        true
    } > "$LAB/etc/bare/meta"
    want_rules=${2-2}
    nsm up bare
    wait_bare || bad "bare did not come up"
}
# respawn -- kill nsm run the hard way and wait for the nanny's next one.
# The nanny spells the whole command line out: -pool and -wait sit between
# the confdir and the verb.
respawn() {
    runpat="nsm -C $LAB/etc -pool .* run bare"
    pkill -9 -f "$runpat" || :
    i=0; while [ $i -lt 20 ] && pgrep -f "$runpat" >/dev/null; do
        sleep 0.25; i=$((i + 1))
    done
    i=0; while [ $i -lt 80 ] && ! pgrep -f "$runpat" >/dev/null; do
        sleep 0.25; i=$((i + 1))
    done
    sleep 2
}
veth_addr() { nsm status bare | sed -n 's/^  veth: ve-bare \([0-9./]*\),.*/\1/p'; }

LAB=$(mktemp -d /tmp/nsm-bare.XXXXXX)
trap 'rm -rf "$LAB"' EXIT
umask 077
KEYS=$LAB/keys; mkdir -m 700 "$KEYS"
want_rules=2

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

# The world: LAN 203.0.113.0/24 (main's), an "internet" segment
# 198.51.100.0/24 where the tunnel server lives, and a second provider
# 100.64.0.0/24. Every probe talks to a gateway running an echo server that
# answers with the peer address it sees, which tells the paths apart: the
# tunnel server NATs to 198.51.100.2, main's LAN address is 203.0.113.2,
# its address at the second provider is 100.64.0.2.
step "topology"
echo_server='
import socket, threading
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 9000)); s.listen(64)
def h(c, a):
    c.sendall(("peer=%s\n" % a[0]).encode()); c.close()
while True:
    c, a = s.accept(); threading.Thread(target=h, args=(c, a), daemon=True).start()
'
for ns in inet srv isp2; do ip netns add "$ns"; done
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
ip link add wan1 type veth peer name lan0 netns isp2
# noprefixroute: main's own table learns nothing about this provider --
# only table 5300 knows the way, so "by table 5300" and "by main's table"
# lead to different places.
ip addr add 100.64.0.2/24 dev wan1 noprefixroute
ip link set wan1 up
ip -n isp2 addr add 100.64.0.1/24 dev lan0
ip -n isp2 link set lan0 up
ip -n isp2 link set lo up
# What NM leaves in main's table: the physical uplink (the first provider).
ip -4 route add default via 203.0.113.1 dev wan0 metric 600
# A second way out, reachable only through its own table.
ip -4 route add default via 100.64.0.1 dev wan1 onlink table 5300
for ns in inet isp2; do ip netns exec "$ns" python3 -u -c "$echo_server" & done
echo "  main=203.0.113.2 (LAN), 100.64.0.2 (isp2); gateways=203.0.113.1, 100.64.0.1"
echo "  internet=198.51.100.1; tunnel server=198.51.100.2"

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
check "main itself has no way to isp2 (only table 5300 does)" "" "$(probe 100.64.0.1)"
check "lappy handshake" yes \
    "$([ "$(wg show lappy latest-handshakes | awk '{print $2}')" -gt 0 ] && echo yes || echo no)"
check "main -> LAN gateway ping" ok "$(ping -c1 -W2 203.0.113.1 >/dev/null 2>&1 && echo ok || echo FAIL)"

# The connection under test: a directory with nothing but meta.
step "nsm run -n: the plan for bare (uplink=main, rule=30200)"
mkdir -p "$LAB/etc/bare" "$LAB/etc/m1"
{ echo "uplink=main"; echo "rule=30200"; echo 'resolv="nameserver 9.9.9.9"'; } > "$LAB/etc/bare/meta"
nsm -n run bare 2>&1 | grep -v '^+ write' | sed 's/^/        /'
check "no run script in the example" 0 "$(ls "$EXAMPLES/bare" | grep -c '^run$')"

step "nsm up bare (uplink=main, rule=30200): the shorthand pair"
nsm up bare
wait_bare || bad "bare did not come up"
nsm status bare | sed 's/^/        /'
check "status: ve0 is the payload" 1 "$(nsm status bare | grep -c '  iface: ve0 (veth) 169.254.100.1/31 up')"
check "status: nsm run holds it" 1 "$(nsm status bare | grep -c '  uplink: held by nsm run (run pid')"
check "status: parts" 1 "$(nsm status bare | grep -c '  veth: ve-bare 169.254.100.0/31, nat, default via 169.254.100.0 dev ve0')"
check "status: rules" 1 "$(nsm status bare | grep -c '  rules: 30200 lookup main, 30201 unreachable')"
check "rules naming the veth in main" 2 "$(count)"
check "bare -> internet seen as (THE point)" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
check "bare -> LAN gateway seen as" peer=203.0.113.2 "$(in_bare nc -w 3 203.0.113.1 9000)"
check "bare -> LAN gateway ping" ok "$(in_bare ping -c1 -W2 203.0.113.1 >/dev/null 2>&1 && echo ok || echo FAIL)"
check "bare -> isp2: main's table has no way, fails closed" "" "$(in_bare nc -w 2 100.64.0.1 9000)"
check "bare resolv.conf" "nameserver 9.9.9.9" "$(in_bare sh -c 'head -1 /etc/resolv.conf')"
check "main -> internet still seen as" peer=198.51.100.2 "$(probe 198.51.100.1)"

step "substrate: a transport born in bare (rides=bare) rides the uplink"
# The run script executes in the transport namespace, like any transport
# that opens its socket there would; nsm run itself is the foreground.
mkdir -p "$LAB/etc/rider"
printf '%s\n' 'rides=bare' > "$LAB/etc/rider/meta"
printf '%s\n' '#!/bin/sh' 'exec nc -w 3 198.51.100.1 9000' > "$LAB/etc/rider/run"
chmod +x "$LAB/etc/rider/run"
check "rides=bare -> internet seen as" peer=203.0.113.2 "$(nsm run rider 2>/dev/null)"

step 'sudo-facing entry: nsm enter (target pinned to $SUDO_UID)'
r=$(SUDO_UID=0 nsm enter bare nc -w 3 198.51.100.1 9000 2>&1)
case $r in
peer=*) check "enter bare -> internet seen as" peer=203.0.113.2 "$r";;
*) echo "  note: enter drops with setgroups, which a rootless user namespace"
   echo "        has denied for good ($r); its network path is exec's, covered above";;
esac

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

step "nsm mounts coexist (m1 exposes 10.48.0.1/32; the pool skips bare's /31)"
# A second connection with a mount. Its "transport" is just the address
# main mounts, owned by the namespace.
printf '%s\n' '#!/bin/sh' \
    'ip -n m1 link add payload0 type dummy 2>/dev/null || :' \
    'ip -n m1 addr replace 10.48.0.1/32 dev payload0' \
    'ip -n m1 link set payload0 up' \
    'exec sleep 1000000' > "$LAB/etc/m1/run"
chmod +x "$LAB/etc/m1/run"
echo 'expose=10.48.0.1/32' > "$LAB/etc/m1/meta"
nsm up m1
i=0
while [ $i -lt 40 ] && ! ip -4 route show 10.48.0.1 2>/dev/null | grep -q .; do
    sleep 0.25; i=$((i + 1))
done
r=$(ip -4 route get 10.48.0.1 | head -1)
case $r in *"via 169.254.100.3 dev ve-m1"*) ok "m1 got the next free /31: $r";; *) bad "main route get mount: $r";; esac
check "main -> mounted address ping" ok "$(ping -c1 -W2 10.48.0.1 >/dev/null 2>&1 && echo ok || echo FAIL)"
check "bare -> mounted address ping (bare follows main's table)" ok \
    "$(in_bare ping -c1 -W2 10.48.0.1 >/dev/null 2>&1 && echo ok || echo FAIL)"

step "no shorthand (rule= absent): nsm touches no rule, bare follows main's rules"
meta_bare "" 0
check "rules naming the veth" 0 "$(count)"
check "no rule -> bare rides the tunnel (bring your own)" peer=198.51.100.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
nsaddr=$(ip -n bare -4 -o addr show dev ve0 | awk '{print $4}' | cut -d/ -f1)
# ... by other means -- the tunnel profile's own ipv4.routing-rules would carry these
ip -4 rule add pref 30200 from "$nsaddr" lookup main
ip -4 rule add pref 30201 from "$nsaddr" unreachable
check "own rules -> seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
nsm down bare
check "down leaves alone the rules it did not make" 2 "$(ip -4 rule | grep -c "^3020[01]:.*from $nsaddr ")"
ip -4 rule del pref 30200; ip -4 rule del pref 30201

step "another table: rule=30200:5300"
meta_bare "30200:5300"
check "status: rules" 1 "$(nsm status bare | grep -c '  rules: 30200 lookup 5300, 30201 unreachable')"
check "bare -> isp2 seen as (through table 5300)" peer=100.64.0.2 "$(in_bare nc -w 3 100.64.0.1 9000)"
check "bare -> LAN gateway: table 5300 has no way there, fails closed" "" "$(in_bare nc -w 2 203.0.113.1 9000)"
check "bare -> internet: table 5300 has no way there, fails closed" "" "$(in_bare nc -w 2 198.51.100.1 9000)"

step "an older value's leftovers: rule=30300; a respawn keeps the veth"
addr_before=$(veth_addr)
{ echo "uplink=main"; echo "rule=30300"; echo 'resolv="nameserver 9.9.9.9"'; } > "$LAB/etc/bare/meta"
respawn
wait_bare || bad "bare did not come back"
check "rules naming the veth" 2 "$(count)"
check "the pair moved" 1 "$(ip -4 rule | grep -c '^30300:.*iif ve-bare lookup main')"
check "nothing left at 30200/30201" 0 "$(ip -4 rule | grep -c '^3020[01]:')"
check "veth kept across the respawn" "$addr_before" "$(veth_addr)"
check "bare -> internet seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
check "status after respawn" "held by nsm run" "$(nsm status bare | sed -n 's/^  uplink: \(held by nsm run\).*/\1/p')"
# ... and the table changes in place: one lookup rule, the new one
{ echo "uplink=main"; echo "rule=30300:5300"; echo 'resolv="nameserver 9.9.9.9"'; } > "$LAB/etc/bare/meta"
respawn
wait_bare || bad "bare did not come back"
check "lookup rules at 30300" "30300: from all iif ve-bare lookup 5300" "$(ip -4 rule | grep '^30300:' | tr -s '\t' ' ')"

step "back to the example's meta, then nsm down bare"
cp "$EXAMPLES/bare/meta" "$LAB/etc/bare/meta"
respawn
wait_bare || bad "bare did not come back"
check "rules naming the veth" 2 "$(count)"
check "bare -> internet seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
nsm status | sed 's/^/        /'
nsm down bare
check "veth gone" 0 "$(ip -4 link show ve-bare 2>/dev/null | wc -l)"
check "rules gone" 0 "$(ip -4 rule | grep -c 've-bare')"
check "chain gone" 0 "$(nft list table ip nsm 2>/dev/null | grep -c 've-bare')"
check "netns gone" 0 "$(ip netns list | grep -c '^bare')"
nsm down m1
check "no chain left in table ip nsm" 0 "$(nft list table ip nsm 2>/dev/null | grep -c 'chain ')"

step "everything SIGKILLed, then down and up again"
nsm up bare
wait_bare || bad "bare did not come up"
pkill -9 -f "nsm -C $LAB/etc" || :
sleep 0.5
nsm down bare
check "rules after down from a killed run" 0 "$(ip -4 rule | grep -c 've-bare')"
check "chain after down from a killed run" 0 "$(nft list table ip nsm 2>/dev/null | grep -c 've-bare')"
check "veth after down from a killed run" 0 "$(ip -4 link show ve-bare 2>/dev/null | wc -l)"
nsm up bare
wait_bare || bad "bare did not come up"
check "rules after up" 2 "$(count)"
check "bare -> internet seen as" peer=203.0.113.2 "$(in_bare nc -w 3 198.51.100.1 9000)"
nsm down bare

step "meta that does not make sense"
mkdir -p "$LAB/etc/bad1" "$LAB/etc/bad2" "$LAB/etc/bad3"
printf 'uplink=main\nexpose=10.0.0.0/8\n' > "$LAB/etc/bad1/meta" # the veth cannot be both
printf 'rule=30200\n' > "$LAB/etc/bad2/meta"                     # no veth to rule on
printf 'rule=0\n' > "$LAB/etc/bad3/meta"
refused() { "$NSM" -C "$LAB/etc" run "$1" >/dev/null 2>&1 && echo admitted || echo refused; }
check "uplink + expose" refused "$(refused bad1)"
check "rule without a veth" refused "$(refused bad2)"
check "rule=0" refused "$(refused bad3)"
rm -f "$LAB/etc/m1/run" # no run script and no uplink: nothing to hold, nothing to run
check "no run script and no uplink" refused "$(refused m1)"

echo
if [ "$fail" != 0 ] && [ -f "$LAB/log.bare" ]; then
    echo "--- nsm log (bare)"; sed 's/^/  | /' "$LAB/log.bare"
fi
echo "passed $pass, failed $fail"
[ "$fail" = 0 ]
INNER
