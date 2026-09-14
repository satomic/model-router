#!/usr/bin/env bash
# Find out why a hostname the host can resolve fails inside the container.
#
#   server-go/scripts/diagnose-dns.sh admin-xxxx-eastus2.openai.azure.com [container]
#
# "no such host" from inside a container is almost never a wrong endpoint. It is the container's
# resolver: a DNS server inherited from the host that the container's network namespace cannot
# reach, a VPN resolver, or an embedded DNS that mishandles a long CNAME chain -- an Azure OpenAI
# endpoint has seven of them. This walks the layers in order so the answer falls out of the
# comparison rather than out of a guess.
set -uo pipefail

HOST="${1:-}"
CONTAINER="${2:-model-router}"
if [[ -z "$HOST" ]]; then
  echo "usage: $0 <hostname> [container name]" >&2
  exit 2
fi

section() { printf '\n=== %s ===\n' "$1"; }

section "1. the host resolves it"
if command -v dscacheutil > /dev/null 2>&1; then
  dscacheutil -q host -a name "$HOST" | sed 's/^/  /'
else
  getent hosts "$HOST" | sed 's/^/  /' || echo "  FAILED on the host too -- the name itself is wrong or your network cannot resolve it"
fi

section "2. what the container was told to use"
docker exec "$CONTAINER" cat /etc/resolv.conf 2>/dev/null | grep -v '^$' | sed 's/^/  /' \
  || echo "  could not read /etc/resolv.conf (is $CONTAINER running?)"
printf '  network: %s\n' "$(docker inspect --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$CONTAINER" 2>/dev/null)"

section "3. the container's own resolver, from a throwaway container on the same network"
NET=$(docker inspect --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' "$CONTAINER" 2>/dev/null)
docker run --rm ${NET:+--network "$NET"} alpine sh -c "
  apk add --no-cache bind-tools > /dev/null 2>&1
  echo '  -- UDP (what a resolver tries first) --'
  dig +short '$HOST' 2>&1 | sed 's/^/    /' || echo '    dig failed'
  echo '  -- was the answer truncated? (a long CNAME chain overflows 512 bytes) --'
  dig +noall +comment '$HOST' 2>&1 | grep -iE 'flags|status' | sed 's/^/    /'
  echo '  -- over TCP, which is the fallback a truncated answer needs --'
  dig +tcp +short '$HOST' 2>&1 | sed 's/^/    /' || echo '    TCP query failed'
"

section "4. the router's own resolver, which is Go's rather than the shell's"
echo "  A shell in the container uses musl; the server uses Go's pure resolver, and the two do"
echo "  not always agree. This is the one that matters:"
docker exec "$CONTAINER" /app/model-router --resolve "$HOST" 2>&1 | sed 's/^/    /' \
  || echo "    (this build has no --resolve; upgrade the image to use it)"

section "reading the result"
cat <<'NOTE'
  1 works, 3 fails            -> the container cannot reach the DNS server it was given.
                                 Run with `--dns 1.1.1.1` (or your own resolver), or put the
                                 container on a network whose DNS it can reach.
  3 UDP fails, 3 TCP works    -> the answer is truncated and the fallback is being dropped.
                                 A different resolver (`--dns`) is the practical fix.
  3 works, 4 fails            -> Go's resolver disagrees with musl's. Report this, with the
                                 output above -- it is a bug worth fixing in the router.
  everything works            -> the failure was transient or has already been fixed; the
                                 router's error message now says which layer failed.
NOTE
