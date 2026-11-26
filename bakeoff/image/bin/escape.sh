#!/usr/bin/env bash
# Escape / hardening checks, run INSIDE a 'locked' sandbox. Prints one JSON line per check.
#   escape.sh <peer_ip> <s3_endpoint>
PEER=${1:-}; S3=${2:-}
r() { printf '{"check":"%s","pass":%s,"detail":"%s"}\n' "$1" "$2" "$(echo "$3" | tr -d '"\n' | cut -c1-120)"; }
fails() { if eval "$2" >/dev/null 2>&1; then r "$1" false "unexpectedly succeeded: $2"; else r "$1" true "blocked"; fi; }
works() { if eval "$2" >/dev/null 2>&1; then r "$1" true "ok"; else r "$1" false "control failed: $2"; fi; }

# runtime
if grep -qi gvisor <(dmesg 2>/dev/null) || grep -q '4.4.0' /proc/version; then r gvisor_runtime true "$(head -c 80 /proc/version)"
else r gvisor_runtime false "$(head -c 80 /proc/version)"; fi
[[ $(id -u) -ne 0 ]] && r non_root true "uid=$(id -u)" || r non_root false "running as root"
[[ ! -e /var/run/secrets/kubernetes.io/serviceaccount/token ]] && r no_sa_token true "absent" || r no_sa_token false "token mounted"
np=$(ls /proc | grep -cE '^[0-9]+$'); [[ $np -lt 30 ]] && r host_procs_hidden true "pids=$np" || r host_procs_hidden false "pids=$np"
fails no_mount "mkdir -p /tmp/m && mount -t tmpfs x /tmp/m"
fails no_sysctl_write "echo 1 > /proc/sys/net/ipv4/ip_forward"
fails no_raw_socket "python3 -c 'import socket; socket.socket(socket.AF_INET, socket.SOCK_RAW, 1)'"
# network
fails metadata_blocked "curl -s -m 3 http://169.254.169.254/"
fails metadata_dns_blocked "curl -s -m 3 -H 'Metadata-Flavor: Google' http://metadata.google.internal/"
fails k8s_api_blocked "curl -sk -m 3 https://${KUBERNETES_SERVICE_HOST:-10.96.0.1}:${KUBERNETES_SERVICE_PORT:-443}/version"
fails internet_blocked "curl -s -m 5 https://pypi.org/"
[[ -n "$PEER" ]] && fails peer_sandbox_blocked "curl -s -m 3 http://$PEER:8000/"
# positive controls (prove the policy isn't just 'network broken')
works dns_works "getent hosts kubernetes.default.svc.cluster.local"
[[ -n "$S3" ]] && works object_store_reachable "curl -s -m 3 -o /dev/null $S3"
