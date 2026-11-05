#!/bin/sh
# Restrict only the Mihomo controller; proxy listeners and SSH are unaffected.
set -eu

tag=cliproxy-network-controller-guard

ensure_rule() {
  tool=$1
  if ! "$tool" -w -C INPUT ! -i lo -p tcp --dport 9090 -m comment --comment "$tag" -j REJECT 2>/dev/null; then
    "$tool" -w -I INPUT 1 ! -i lo -p tcp --dport 9090 -m comment --comment "$tag" -j REJECT
  fi
}

remove_rule() {
  tool=$1
  while "$tool" -w -C INPUT ! -i lo -p tcp --dport 9090 -m comment --comment "$tag" -j REJECT 2>/dev/null; do
    "$tool" -w -D INPUT ! -i lo -p tcp --dport 9090 -m comment --comment "$tag" -j REJECT
  done
}

case "${1:-}" in
  start)
    ensure_rule /usr/sbin/iptables
    if ! ensure_rule /usr/sbin/ip6tables; then
      remove_rule /usr/sbin/iptables
      exit 1
    fi
    ;;
  stop)
    remove_rule /usr/sbin/ip6tables
    remove_rule /usr/sbin/iptables
    ;;
  *)
    printf 'Usage: %s {start|stop}\n' "$0" >&2
    exit 2
    ;;
esac
