# Conditional DNS forwarding of one domain (consul) to the DNS interface of
# the local Consul agent: consul.dns_forwarding of norca-deployer. Consul
# answers that domain; every other name keeps being answered by the servers
# the host has (systemd-resolved asks the global DNS= server too, in
# parallel, and Consul refuses those names). The resolver must be there
# already (systemd-resolved or dnsmasq); nothing is installed here. On a
# node with OpenRC (Alpine) dnsmasq is the only resolver: there is no
# systemd-resolved, and rc-service starts and restarts dnsmasq.
#
# One implementation for both ways the deployer reaches a host: the SSH path
# uploads this file and runs it with sh, the install script of a scaled
# machine embeds it and calls norca_consul_dns. POSIX sh only (dash, busybox
# ash): no bash features and no `local` (the variables start with ncd_). It
# does not rely on `set -e` (every command that can fail is checked) and sets
# no trap (the machine script has its own).
#
# Parameters (variables set by the caller):
#   NCD_ENABLED         1: forward the domain; otherwise remove what an
#                       earlier run wrote
#   NCD_DOMAIN          the domain (consul)
#   NCD_RESOLVED_DNS    the Consul DNS in the DNS= form of resolved.conf
#                       (127.0.0.1:8600, [::1]:8600)
#   NCD_DNSMASQ_SERVER  the same in the server= form of dnsmasq
#                       (127.0.0.1#8600)
#   NCD_PORT            its port
#   NCD_METHOD          auto, systemd-resolved or dnsmasq
#   NCD_DOCKER          1: containers on docker0 resolve the domain too
#                       (systemd-resolved only)
#   NCD_INIT            the init system of the node, systemd or openrc: the
#                       facts of the node over SSH, the family of the machine
#                       in the install script
#   NCD_ROOT            prefix of every path read or written (tests); empty
#                       on a host
#
# Output, one line each: "method M", "changed PATH", "unchanged PATH",
# "removed PATH", "restarted UNIT", "bridge ADDRESS/PREFIX" (docker0),
# "warn TEXT", "error TEXT". The text of an error ends a sentence whose
# subject is the host ("has no local resolver ..."). After an error the
# function returns 1; every check that can fail runs before the first file
# is written.

ncd_say() {
  printf '%s\n' "$*"
}

ncd_error() {
  ncd_say "error $*"
}

# ncd_has WORD LIST reports whether a space separated list has the word.
ncd_has() {
  case " $2 " in
  *" $1 "*) return 0 ;;
  esac
  return 1
}

# ncd_installed UNIT reports whether the program of a resolver is there.
ncd_installed() {
  case $1 in
  systemd-resolved)
    [ -x "$ncd_root/lib/systemd/systemd-resolved" ] || [ -x "$ncd_root/usr/lib/systemd/systemd-resolved" ]
    ;;
  dnsmasq)
    [ -x "$ncd_root/usr/sbin/dnsmasq" ] || [ -x "$ncd_root/sbin/dnsmasq" ]
    ;;
  *)
    return 1
    ;;
  esac
}

# ncd_active UNIT reports whether a service runs.
ncd_active() {
  if [ "$ncd_init" = openrc ]; then
    rc-service "$1" status >/dev/null 2>&1
  else
    systemctl is-active --quiet "$1" >/dev/null 2>&1
  fi
}

# ncd_enabled UNIT reports whether a service starts at boot: an enabled
# unit, or a service in a runlevel of OpenRC (a link that rc-update add
# made, to the absolute path of the service).
ncd_enabled() {
  if [ "$ncd_init" != openrc ]; then
    systemctl is-enabled --quiet "$1" >/dev/null 2>&1
    return
  fi
  for ncd_link in "$ncd_root"/etc/runlevels/*/"$1"; do
    if [ -e "$ncd_link" ] || [ -L "$ncd_link" ]; then
      return 0
    fi
  done
  return 1
}

# ncd_state UNIT prints "active", "inactive" or "not installed". While the
# init system does not run (an image built in a container), a service that
# starts at boot counts as active.
ncd_state() {
  if [ "$ncd_online" = 1 ]; then
    if ncd_active "$1"; then
      ncd_say active
      return 0
    fi
  elif ncd_enabled "$1"; then
    ncd_say active
    return 0
  fi
  if ncd_installed "$1"; then
    ncd_say inactive
  else
    ncd_say "not installed"
  fi
}

# ncd_detect reads what the host has: the resolvers, the version of systemd,
# the name servers of /etc/resolv.conf and the hosts line of nsswitch.conf.
# The init system runs when systemd made /run/systemd/system, or when OpenRC
# booted the host (/run/openrc/softlevel, the file rc-service looks for).
ncd_detect() {
  ncd_online=0
  ncd_systemd=0
  if [ "$ncd_init" = openrc ]; then
    if [ -e "$ncd_root/run/openrc/softlevel" ]; then
      ncd_online=1
    fi
    ncd_resolved="not installed"
  else
    if [ -d "$ncd_root/run/systemd/system" ]; then
      ncd_online=1
    fi
    ncd_resolved=$(ncd_state systemd-resolved)
    ncd_systemd=$(systemctl --version 2>/dev/null | awk 'NR == 1 { print $2 }') || ncd_systemd=
    ncd_systemd=${ncd_systemd%%[!0-9]*}
    if [ -z "$ncd_systemd" ]; then
      ncd_systemd=0
    fi
  fi
  ncd_dnsmasq=$(ncd_state dnsmasq)
  ncd_ns=$(awk '$1 == "nameserver" { printf "%s ", $2 }' "$ncd_root/etc/resolv.conf" 2>/dev/null) || ncd_ns=
  ncd_ns=${ncd_ns% }
  ncd_hosts=$(awk '$1 == "hosts:" { $1 = ""; print; exit }' "$ncd_root/etc/nsswitch.conf" 2>/dev/null) || ncd_hosts=
}

# ncd_via_resolved reports whether the lookups of the host go through
# systemd-resolved: its stub in /etc/resolv.conf or its NSS module. Without
# a running systemd the files are those of the build, not of the machine,
# and are not looked at.
ncd_via_resolved() {
  [ "$ncd_online" = 1 ] || return 0
  ncd_has 127.0.0.53 "$ncd_ns" || ncd_has 127.0.0.54 "$ncd_ns" || ncd_has resolve "$ncd_hosts"
}

# ncd_via_dnsmasq reports whether /etc/resolv.conf names a local dnsmasq.
ncd_via_dnsmasq() {
  [ "$ncd_online" = 1 ] || return 0
  ncd_has 127.0.0.1 "$ncd_ns" || ncd_has ::1 "$ncd_ns"
}

ncd_found_resolved() {
  if [ "$ncd_resolved" = active ]; then
    printf 'systemd-resolved active but not used for lookups (no 127.0.0.53 in /etc/resolv.conf, no resolve in the hosts line of /etc/nsswitch.conf)'
  else
    printf 'systemd-resolved %s' "$ncd_resolved"
  fi
}

ncd_found_dnsmasq() {
  if [ "$ncd_dnsmasq" = active ]; then
    printf 'dnsmasq active but not named in /etc/resolv.conf (no 127.0.0.1)'
  else
    printf 'dnsmasq %s' "$ncd_dnsmasq"
  fi
}

ncd_found_ns() {
  if [ -n "$ncd_ns" ]; then
    printf '/etc/resolv.conf names %s' "$ncd_ns"
  else
    printf '/etc/resolv.conf names no server'
  fi
}

ncd_version() {
  if [ "$ncd_systemd" = 0 ]; then
    printf 'systemd of an unknown version'
  else
    printf 'systemd %s' "$ncd_systemd"
  fi
}

# ncd_choose_openrc sets ncd_method on an OpenRC node: dnsmasq, the only
# resolver there, for auto and dnsmasq; systemd-resolved and the containers
# (which need it) are refused.
ncd_choose_openrc() {
  if [ "${NCD_METHOD:-auto}" = systemd-resolved ]; then
    ncd_error "cannot forward the $ncd_domain domain with systemd-resolved (consul.dns_forwarding.method): the node runs OpenRC, which has no systemd-resolved; use method auto or dnsmasq"
    return 1
  fi
  if [ "${NCD_DOCKER:-0}" = 1 ]; then
    ncd_error "cannot let containers resolve the $ncd_domain domain (consul.dns_forwarding.docker): the node runs OpenRC, and the containers need systemd-resolved; set docker = false"
    return 1
  fi
  if [ "$ncd_dnsmasq" != active ] || ! ncd_via_dnsmasq; then
    ncd_remedy="start it (rc-update add dnsmasq default; rc-service dnsmasq start) and name 127.0.0.1 in /etc/resolv.conf"
    if [ "${NCD_METHOD:-auto}" = dnsmasq ]; then
      ncd_error "cannot forward the $ncd_domain domain with dnsmasq (consul.dns_forwarding.method): $(ncd_found_dnsmasq), $(ncd_found_ns). Install dnsmasq with host_packages, $ncd_remedy"
    else
      ncd_error "has no local resolver the deployer can forward the $ncd_domain domain from: $(ncd_found_dnsmasq), $(ncd_found_ns). OpenRC has no systemd-resolved: install dnsmasq with host_packages, $ncd_remedy, or set consul.dns_forwarding.enabled = false"
    fi
    return 1
  fi
  ncd_method=dnsmasq
  ncd_say "method $ncd_method"
  if [ "$ncd_online" != 1 ]; then
    ncd_say "warn OpenRC was not started (an image built in a container?): dnsmasq is chosen by its runlevel, /etc/resolv.conf is not checked, and nothing is restarted"
  fi
}

# ncd_choose sets ncd_method from NCD_METHOD and what the host has.
ncd_choose() {
  if [ "$ncd_init" = openrc ]; then
    ncd_choose_openrc
    return
  fi
  ncd_ok_resolved=0
  if [ "$ncd_resolved" = active ] && ncd_via_resolved; then
    ncd_ok_resolved=1
  fi
  ncd_ok_dnsmasq=0
  if [ "$ncd_dnsmasq" = active ] && ncd_via_dnsmasq; then
    ncd_ok_dnsmasq=1
  fi
  case ${NCD_METHOD:-auto} in
  systemd-resolved)
    if [ "$ncd_ok_resolved" != 1 ]; then
      ncd_error "cannot forward the $ncd_domain domain with systemd-resolved (consul.dns_forwarding.method): $(ncd_found_resolved), $(ncd_found_ns). Install systemd-resolved with host_packages (it takes over /etc/resolv.conf), or choose another method"
      return 1
    fi
    ncd_method=systemd-resolved
    ;;
  dnsmasq)
    if [ "$ncd_ok_dnsmasq" != 1 ]; then
      ncd_error "cannot forward the $ncd_domain domain with dnsmasq (consul.dns_forwarding.method): $(ncd_found_dnsmasq), $(ncd_found_ns). Install dnsmasq with host_packages and name 127.0.0.1 in /etc/resolv.conf, or choose another method"
      return 1
    fi
    ncd_method=dnsmasq
    ;;
  *)
    if [ "$ncd_ok_resolved" = 1 ]; then
      ncd_method=systemd-resolved
    elif [ "$ncd_ok_dnsmasq" = 1 ]; then
      ncd_method=dnsmasq
    else
      ncd_error "has no local resolver the deployer can forward the $ncd_domain domain from: $(ncd_found_resolved), $(ncd_found_dnsmasq), $(ncd_found_ns). Install systemd-resolved or dnsmasq with host_packages, or set consul.dns_forwarding.enabled = false"
      return 1
    fi
    ;;
  esac
  ncd_say "method $ncd_method"
  if [ "$ncd_online" != 1 ]; then
    ncd_say "warn systemd does not run (an image built in a container?): $ncd_method is chosen by its enabled unit, /etc/resolv.conf is not checked, and nothing is restarted"
  fi
}

# ncd_same FILE1 FILE2 is true when the files have the same content. It
# compares their SHA-256: cmp is not on a minimal RHEL (no diffutils).
ncd_same() {
  ncd_sum1=$(sha256sum <"$1") && ncd_sum2=$(sha256sum <"$2") && [ "$ncd_sum1" = "$ncd_sum2" ]
}

# ncd_put PATH MODE FILE writes the content of FILE to PATH unless PATH has
# it already; ncd_wrote is 1 when it wrote.
ncd_put() {
  ncd_wrote=0
  ncd_dst=$ncd_root$1
  if [ -f "$ncd_dst" ] && ncd_same "$3" "$ncd_dst"; then
    ncd_say "unchanged $1"
    return 0
  fi
  ncd_dir=$(dirname "$ncd_dst")
  if mkdir -p "$ncd_dir" && cp "$3" "$ncd_dst.norca-new" && chmod "$2" "$ncd_dst.norca-new" &&
    mv -f "$ncd_dst.norca-new" "$ncd_dst"; then
    ncd_wrote=1
    ncd_say "changed $1"
    return 0
  fi
  rm -f "$ncd_dst.norca-new"
  ncd_error "could not write $1"
  return 1
}

# ncd_remove PATH removes a file that an earlier run wrote; ncd_wrote is 1
# when it was there.
ncd_remove() {
  ncd_wrote=0
  if [ ! -e "$ncd_root$1" ] && [ ! -L "$ncd_root$1" ]; then
    return 0
  fi
  if rm -f "$ncd_root$1"; then
    ncd_wrote=1
    ncd_say "removed $1"
    return 0
  fi
  ncd_error "could not remove $1"
  return 1
}

# ncd_restart UNIT restarts a running service (rc-service on OpenRC; its
# reload of dnsmasq does not read the configuration again). Without a
# running init system the configuration applies at the next boot.
ncd_restart() {
  if [ "$ncd_online" != 1 ] || ! ncd_active "$1"; then
    return 0
  fi
  if [ "$ncd_init" = openrc ]; then
    if ncd_out=$(rc-service "$1" restart 2>&1); then
      ncd_say "restarted $1"
      return 0
    fi
  elif ncd_out=$(systemctl restart "$1" 2>&1); then
    ncd_say "restarted $1"
    return 0
  fi
  ncd_error "could not restart $1: $(printf '%s' "$ncd_out" | tr '\n' ' ')"
  return 1
}

# ncd_warn_daemon_json warns about the dns key of daemon.json once
# systemd-resolved no longer listens on docker0.
ncd_warn_daemon_json() {
  if grep -q '"dns"' "$ncd_root$ncd_daemon_json" 2>/dev/null; then
    ncd_say "warn $ncd_daemon_json keeps its \"dns\" key, but systemd-resolved no longer listens on docker0: containers cannot resolve names through it; remove the key and restart docker"
  fi
}

# ncd_docker_json puts the daemon.json with "dns": [docker0 address] into
# $ncd_tmp/daemon.json, every other key kept; ncd_json_same is 1 when the
# file has that already. A daemon.json that does not parse is refused.
ncd_docker_json() {
  ncd_json_same=0
  ncd_dj=$ncd_root$ncd_daemon_json
  printf '{\n  "dns": ["%s"]\n}\n' "$ncd_bridge_addr" >"$ncd_tmp/daemon.json"
  if [ ! -f "$ncd_dj" ] || ! grep -q '[^[:space:]]' "$ncd_dj" 2>/dev/null; then
    return 0
  fi
  # The file this function made: no tool is needed to see it is unchanged.
  if ncd_same "$ncd_tmp/daemon.json" "$ncd_dj"; then
    ncd_json_same=1
    return 0
  fi
  if command -v jq >/dev/null 2>&1; then
    if ! jq -e 'type == "object"' "$ncd_dj" >/dev/null 2>&1; then
      ncd_error "has a $ncd_daemon_json that is not a JSON object; fix it, the deployer adds \"dns\" to it for consul.dns_forwarding.docker"
      return 1
    fi
    if jq -e --arg a "$ncd_bridge_addr" '.dns == [$a]' "$ncd_dj" >/dev/null 2>&1; then
      ncd_json_same=1
      return 0
    fi
    if jq --arg a "$ncd_bridge_addr" '.dns = [$a]' "$ncd_dj" >"$ncd_tmp/daemon.json" 2>/dev/null; then
      return 0
    fi
    ncd_error "could not add \"dns\" to $ncd_daemon_json with jq"
    return 1
  fi
  if command -v python3 >/dev/null 2>&1; then
    # Exit 2: no JSON object, 3: dns is set already.
    ncd_py='import json, sys
path, addr, out = sys.argv[1:4]
try:
    with open(path) as f:
        data = json.load(f)
except ValueError:
    sys.exit(2)
if not isinstance(data, dict):
    sys.exit(2)
if data.get("dns") == [addr]:
    sys.exit(3)
data["dns"] = [addr]
with open(out, "w") as f:
    json.dump(data, f, indent=2)
    f.write("\n")'
    if python3 -c "$ncd_py" "$ncd_dj" "$ncd_bridge_addr" "$ncd_tmp/daemon.json" 2>/dev/null; then
      ncd_rc=0
    else
      ncd_rc=$?
    fi
    case $ncd_rc in
    0) return 0 ;;
    3)
      ncd_json_same=1
      return 0
      ;;
    2)
      ncd_error "has a $ncd_daemon_json that is not a JSON object; fix it, the deployer adds \"dns\" to it for consul.dns_forwarding.docker"
      return 1
      ;;
    esac
    ncd_error "could not add \"dns\" to $ncd_daemon_json with python3"
    return 1
  fi
  ncd_error "cannot add \"dns\" to $ncd_daemon_json for consul.dns_forwarding.docker: neither jq nor python3 is installed. Install jq with host_packages"
  return 1
}

# ncd_prepare_resolved checks what systemd-resolved needs and finds the
# docker0 address.
ncd_prepare_resolved() {
  if [ "${NCD_PORT:-53}" != 53 ] && [ "$ncd_systemd" -lt 246 ]; then
    ncd_error "runs $(ncd_version): systemd-resolved sends to a DNS server on a port other than 53 ($NCD_RESOLVED_DNS) from systemd 246 on. Use dnsmasq (consul.dns_forwarding.method = dnsmasq), or let Consul answer DNS on port 53"
    return 1
  fi
  ncd_docker=0
  ncd_bridge=
  ncd_bridge_addr=
  if [ "${NCD_DOCKER:-0}" != 1 ]; then
    return 0
  fi
  if [ "$ncd_systemd" -lt 247 ]; then
    ncd_error "runs $(ncd_version): consul.dns_forwarding.docker needs DNSStubListenerExtra= of systemd 247 or newer; set docker = false"
    return 1
  fi
  ncd_bridge=$(ip -4 -o addr show dev docker0 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "inet") { print $(i + 1); exit } }') || ncd_bridge=
  if [ -z "$ncd_bridge" ]; then
    if [ "$ncd_online" != 1 ]; then
      ncd_say "warn docker0 is not there while systemd does not run: the containers are left out; run the script again on a running machine for them"
      return 0
    fi
    ncd_error "has no docker0 bridge with an IPv4 address for consul.dns_forwarding.docker (is Docker installed and running?)"
    return 1
  fi
  ncd_bridge_addr=${ncd_bridge%/*}
  ncd_docker=1
  ncd_docker_json
}

# ncd_write_resolved writes the drop-in and the negative trust anchor and
# restarts systemd-resolved when one of them changed.
ncd_write_resolved() {
  {
    printf '; Managed by norca-deployer. Do not edit.\n'
    printf '; consul.dns_forwarding: no DNSSEC validation below the %s domain, whose\n' "$ncd_domain"
    printf '; answers the Consul agent does not sign.\n'
    printf '%s\n' "$ncd_domain"
    printf '; A .negative file replaces the built-in negative trust anchors of\n'
    printf '; systemd-resolved, so they follow (the list of systemd 252).\n'
    for ncd_nta in home.arpa 10.in-addr.arpa 16.172.in-addr.arpa 17.172.in-addr.arpa 18.172.in-addr.arpa \
      19.172.in-addr.arpa 20.172.in-addr.arpa 21.172.in-addr.arpa 22.172.in-addr.arpa 23.172.in-addr.arpa \
      24.172.in-addr.arpa 25.172.in-addr.arpa 26.172.in-addr.arpa 27.172.in-addr.arpa 28.172.in-addr.arpa \
      29.172.in-addr.arpa 30.172.in-addr.arpa 31.172.in-addr.arpa 170.0.0.192.in-addr.arpa \
      171.0.0.192.in-addr.arpa 168.192.in-addr.arpa d.f.ip6.arpa ipv4only.arpa corp home internal intranet \
      lan local private test; do
      printf '%s\n' "$ncd_nta"
    done
  } >"$ncd_tmp/anchor"
  {
    printf '# Managed by norca-deployer. Do not edit.\n'
    printf '# consul.dns_forwarding: the Consul agent answers the %s domain; other\n' "$ncd_domain"
    printf '# names are answered by the servers the host has.\n'
    printf '[Resolve]\n'
    printf 'DNS=%s\n' "$NCD_RESOLVED_DNS"
    printf 'Domains=~%s\n' "$ncd_domain"
    if [ "$ncd_docker" = 1 ]; then
      printf 'DNSStubListenerExtra=%s\n' "$ncd_bridge_addr"
    fi
  } >"$ncd_tmp/dropin"
  ncd_had_extra=0
  if grep -q '^DNSStubListenerExtra=' "$ncd_root$ncd_dropin" 2>/dev/null; then
    ncd_had_extra=1
  fi
  ncd_changed=0
  ncd_put "$ncd_anchor" 0644 "$ncd_tmp/anchor" || return 1
  [ "$ncd_wrote" = 0 ] || ncd_changed=1
  ncd_put "$ncd_dropin" 0644 "$ncd_tmp/dropin" || return 1
  [ "$ncd_wrote" = 0 ] || ncd_changed=1
  if [ "$ncd_changed" = 1 ]; then
    ncd_restart systemd-resolved || return 1
  fi
  ncd_check_upstream
  if [ "$ncd_docker" = 1 ]; then
    ncd_say "bridge $ncd_bridge"
    ncd_write_docker
    return
  fi
  if [ "$ncd_had_extra" = 1 ]; then
    ncd_warn_daemon_json
  fi
}

# ncd_check_upstream warns when systemd-resolved has no DNS server but the
# Consul agent: names outside the domain then do not resolve (and the global
# server of the drop-in keeps the fallback servers of resolved unused). On
# Debian with ifupdown, a systemd-resolved installed after the network came
# up gets the DHCP servers only with the next lease.
ncd_check_upstream() {
  [ "$ncd_online" = 1 ] || return 0
  ncd_servers=$(resolvectl dns 2>/dev/null) || return 0
  ncd_others=$(printf '%s\n' "$ncd_servers" | awk -v own="$NCD_RESOLVED_DNS" '
    { sub(/^[^:]*:/, ""); for (i = 1; i <= NF; i++) if ($i != own) n++ }
    END { print n + 0 }') || ncd_others=1
  if [ "$ncd_others" = 0 ]; then
    ncd_say "warn systemd-resolved has no DNS server but the Consul agent (resolvectl dns): names outside the $ncd_domain domain do not resolve. Give it the servers of the network: with ifupdown and DHCP they come with the next lease (ifdown and ifup the interface, or reboot), or set DNS= in a file of /etc/systemd/resolved.conf.d"
  fi
}

# ncd_write_docker writes daemon.json and restarts docker when it changed.
ncd_write_docker() {
  if [ "$ncd_json_same" = 1 ]; then
    ncd_say "unchanged $ncd_daemon_json"
    return 0
  fi
  ncd_put "$ncd_daemon_json" 0644 "$ncd_tmp/daemon.json" || return 1
  if [ "$ncd_wrote" = 1 ] && [ "$ncd_online" = 1 ] && systemctl is-active --quiet docker >/dev/null 2>&1; then
    ncd_restart docker || return 1
    ncd_say "warn docker was restarted to read the dns of $ncd_daemon_json: its running containers were restarted unless live-restore is on"
  fi
}

# ncd_dnsmasq_cmdline prints the command lines of the running dnsmasq, one
# per line: the main process of its unit, or on OpenRC the process of its
# pid file (the service of Alpine has start-stop-daemon write
# /run/dnsmasq.pid), else those pidof finds (under supervise-daemon).
ncd_dnsmasq_cmdline() {
  if [ "$ncd_init" = openrc ]; then
    ncd_pids=$(cat "$ncd_root/run/dnsmasq.pid" 2>/dev/null) || ncd_pids=
    if [ -z "$ncd_pids" ]; then
      ncd_pids=$(pidof dnsmasq 2>/dev/null) || ncd_pids=
    fi
  else
    ncd_pids=$(systemctl show -p MainPID --value dnsmasq 2>/dev/null) || ncd_pids=
  fi
  for ncd_pid in $ncd_pids; do
    case $ncd_pid in
    0 | *[!0-9]*) ;;
    *)
      tr '\000' ' ' <"$ncd_root/proc/$ncd_pid/cmdline" 2>/dev/null
      printf '\n'
      ;;
    esac
  done
}

# ncd_prepare_dnsmasq checks that dnsmasq reads /etc/dnsmasq.d: a conf-dir
# line in /etc/dnsmasq.conf (the package of Alpine has it), or on the
# command line of the running dnsmasq (the unit of Debian passes it).
ncd_prepare_dnsmasq() {
  if grep -Eq '^[[:space:]]*conf-dir=/etc/dnsmasq\.d/?(,|[[:space:]]*$)' "$ncd_root/etc/dnsmasq.conf" 2>/dev/null; then
    return 0
  fi
  if [ "$ncd_online" = 1 ]; then
    if ncd_dnsmasq_cmdline | grep -Eq -- '(--conf-dir=|-7 ?)/etc/dnsmasq\.d/?(,| |$)'; then
      return 0
    fi
  elif [ "$ncd_init" = openrc ]; then
    # The options of the service of Alpine (command_args).
    if grep -q '^[^#]*/etc/dnsmasq\.d' "$ncd_root/etc/conf.d/dnsmasq" 2>/dev/null; then
      return 0
    fi
  elif grep -q '/etc/dnsmasq\.d' "$ncd_root/etc/init.d/dnsmasq" 2>/dev/null; then
    return 0
  fi
  ncd_hint=
  if [ "$ncd_init" = openrc ]; then
    ncd_hint=" (the dnsmasq.conf of the Alpine package has that line)"
  fi
  ncd_error "runs a dnsmasq that does not read /etc/dnsmasq.d (no conf-dir=/etc/dnsmasq.d in /etc/dnsmasq.conf or on its command line); add conf-dir=/etc/dnsmasq.d/,*.conf to /etc/dnsmasq.conf$ncd_hint"
  return 1
}

# ncd_check_dnsmasq_upstream warns on OpenRC when dnsmasq has no server for
# the names outside the domain: it reads /etc/resolv.conf, which names only
# dnsmasq itself (127.0.0.1, ::1; dnsmasq skips an address of its own:
# "ignoring nameserver 127.0.0.1 - local interface", and answers REFUSED),
# and no resolv-file= or server= in its configuration or on its command
# line gives it another. On Alpine udhcpc writes the servers of the network
# into /etc/resolv.conf unless RESOLV_CONF in /etc/udhcpc/udhcpc.conf names
# another file.
ncd_check_dnsmasq_upstream() {
  if [ "$ncd_init" != openrc ] || [ "$ncd_online" != 1 ]; then
    return 0
  fi
  for ncd_server in $ncd_ns; do
    case $ncd_server in
    127.0.0.1 | ::1) ;;
    *) return 0 ;;
    esac
  done
  if cat "$ncd_root/etc/dnsmasq.conf" "$ncd_root"/etc/dnsmasq.d/*.conf 2>/dev/null |
    grep -Eq '^[[:space:]]*(resolv-file=|server=[^/])'; then
    return 0
  fi
  if ncd_dnsmasq_cmdline | grep -Eq -- '(^| )(--resolv-file=|-r |--server=[^/]|-S [^/])'; then
    return 0
  fi
  ncd_say "warn dnsmasq has no DNS server for names outside the $ncd_domain domain: /etc/resolv.conf names only $ncd_ns, which dnsmasq does not ask, and no resolv-file= or server= gives it another. Give it the servers of the network: with udhcpc, RESOLV_CONF=/etc/resolv.dnsmasq in /etc/udhcpc/udhcpc.conf and resolv-file=/etc/resolv.dnsmasq in a file of /etc/dnsmasq.d"
}

# ncd_write_dnsmasq writes the server line of the domain and restarts
# dnsmasq when it changed.
ncd_write_dnsmasq() {
  {
    printf '# Managed by norca-deployer. Do not edit.\n'
    printf '# consul.dns_forwarding: the %s domain goes to the Consul agent; other\n' "$ncd_domain"
    printf '# names keep going to the servers dnsmasq has.\n'
    printf 'server=/%s/%s\n' "$ncd_domain" "$NCD_DNSMASQ_SERVER"
  } >"$ncd_tmp/dnsmasq"
  ncd_put "$ncd_dnsmasq_conf" 0644 "$ncd_tmp/dnsmasq" || return 1
  if [ "$ncd_wrote" = 1 ]; then
    ncd_restart dnsmasq || return 1
  fi
  ncd_check_dnsmasq_upstream
}

# ncd_remove_resolved removes the files of systemd-resolved and restarts it
# when one was there.
ncd_remove_resolved() {
  ncd_had_extra=0
  if grep -q '^DNSStubListenerExtra=' "$ncd_root$ncd_dropin" 2>/dev/null; then
    ncd_had_extra=1
  fi
  ncd_changed=0
  ncd_remove "$ncd_dropin" || return 1
  [ "$ncd_wrote" = 0 ] || ncd_changed=1
  ncd_remove "$ncd_anchor" || return 1
  [ "$ncd_wrote" = 0 ] || ncd_changed=1
  if [ "$ncd_changed" = 1 ]; then
    ncd_restart systemd-resolved || return 1
  fi
  if [ "$ncd_had_extra" = 1 ]; then
    ncd_warn_daemon_json
  fi
}

# ncd_remove_dnsmasq removes the file of dnsmasq and restarts it when it was
# there.
ncd_remove_dnsmasq() {
  ncd_remove "$ncd_dnsmasq_conf" || return 1
  if [ "$ncd_wrote" = 1 ]; then
    ncd_restart dnsmasq || return 1
  fi
}

ncd_run() {
  case $ncd_init in
  systemd | openrc) ;;
  *)
    ncd_error "was given the init system \"$ncd_init\" (NCD_INIT), not systemd or openrc"
    return 1
    ;;
  esac
  ncd_detect
  if [ "${NCD_ENABLED:-0}" != 1 ]; then
    ncd_remove_resolved || return 1
    ncd_remove_dnsmasq
    return
  fi
  ncd_choose || return 1
  if [ "$ncd_method" = systemd-resolved ]; then
    ncd_prepare_resolved || return 1
    ncd_write_resolved || return 1
    ncd_remove_dnsmasq
    return
  fi
  if [ "${NCD_DOCKER:-0}" = 1 ]; then
    ncd_error "cannot let containers resolve the $ncd_domain domain with dnsmasq: consul.dns_forwarding.docker needs systemd-resolved (DNSStubListenerExtra=)"
    return 1
  fi
  ncd_prepare_dnsmasq || return 1
  ncd_write_dnsmasq || return 1
  ncd_remove_resolved
}

# norca_consul_dns configures the forwarding (see the top of the file).
norca_consul_dns() {
  ncd_root=${NCD_ROOT:-}
  ncd_init=${NCD_INIT:-}
  ncd_domain=${NCD_DOMAIN:-consul}
  ncd_dropin=/etc/systemd/resolved.conf.d/norca-consul.conf
  ncd_anchor=/etc/dnssec-trust-anchors.d/norca-consul.negative
  ncd_dnsmasq_conf=/etc/dnsmasq.d/norca-consul.conf
  ncd_daemon_json=/etc/docker/daemon.json
  if ! ncd_tmp=$(mktemp -d 2>/dev/null); then
    ncd_error "could not make a temporary directory"
    return 1
  fi
  if ncd_run; then
    ncd_rc=0
  else
    ncd_rc=1
  fi
  rm -rf "$ncd_tmp"
  return "$ncd_rc"
}
