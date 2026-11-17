#!/usr/bin/env bash
# The Alias rehearsal's one record: a throwaway name under
# omarchy-m-testing.org as a CNAME of the Lab address, at Porkbun, which
# holds that domain's DNS until the day's step 19. It plays the record the
# other side adds for the Alias: Porkbun's DNS is DNS only by nature.
#
#   rehearsal-record.sh --name HOST                    print the API calls; send nothing
#   rehearsal-record.sh --check --name HOST            the record is there; else exit 1
#   rehearsal-record.sh --apply --name HOST            make it when missing
#   rehearsal-record.sh --apply --remove --name HOST   delete it; with --check,
#                                                      exit 1 while it is still there
#
#   --name HOST   one label under omarchy-m-testing.org, such as
#                 alias-rehearsal.omarchy-m-testing.org; never www or the
#                 domain itself, which are the old site's
#
# A record of that name that is not a CNAME of the Lab address is not the
# rehearsal's: it is neither written over nor deleted. Porkbun's two keys
# come from --porkbun-keys FILE, a file only you can read (see lib.sh);
# Cloudflare is not called.

# shellcheck source=scripts/cloud/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
parse_mode "$@"
set -- ${ARGS[@]+"${ARGS[@]}"}

REHEARSAL_DOMAIN="omarchy-m-testing.org"
NAME=""
WANT=present
while (( $# )); do
  case "$1" in
    --name)
      [[ $# -ge 2 ]] || usage 2 21
      NAME="$2"
      shift
      ;;
    --remove) WANT=absent ;;
    *) usage 2 21 ;;
  esac
  shift
done
# A dry run with no name shows the calls for the name the rehearsal uses.
[[ $MODE != dry-run || -n $NAME ]] || NAME="alias-rehearsal.$REHEARSAL_DOMAIN"
[[ $NAME =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.omarchy-m-testing\.org$ && $NAME != "www.$REHEARSAL_DOMAIN" ]] ||
  die "--name takes one throwaway label under $REHEARSAL_DOMAIN, such as alias-rehearsal.$REHEARSAL_DOMAIN. Nothing was sent"
SUB="${NAME%."$REHEARSAL_DOMAIN"}"
TTL=600

record_body() { jq -nc --arg n "$SUB" --arg c "$LAB_HOST" --arg t "$TTL" '{name: $n, type: "CNAME", content: $c, ttl: $t}'; }

if [[ $MODE == dry-run ]]; then
  say "$ME: a dry run. These are the calls --apply makes; nothing is sent."
  say
  say "the record of $NAME, at Porkbun"
  would "POST $PORKBUN_API/dns/retrieveByNameType/$REHEARSAL_DOMAIN/CNAME/$SUB {}"
  if [[ $WANT == absent ]]; then
    would "POST $PORKBUN_API/dns/deleteByNameType/$REHEARSAL_DOMAIN/CNAME/$SUB {}"
  else
    would "POST $PORKBUN_API/dns/create/$REHEARSAL_DOMAIN $(record_body)"
  fi
  exit 0
fi

DRIFTED=()
drifted() {
  DRIFTED+=("$1")
  say "drifted  $1: $2"
}

# have: how many CNAME records carry the name, and what the first leads to.
have() {
  local out
  out="$(pb -X POST -d '{}' "$PORKBUN_API/dns/retrieveByNameType/$REHEARSAL_DOMAIN/CNAME/$SUB")" || die "Porkbun gave no answer"
  pb_ok "$out" || die "Porkbun's lookup of $NAME failed: $(printf '%s' "$out" | jq -c '{status, message}' 2>/dev/null || echo "no JSON in the answer")"
  COUNT="$(printf '%s' "$out" | jq '.records | length')"
  CONTENT="$(printf '%s' "$out" | jq -r '.records[0].content // empty' | tr '[:upper:]' '[:lower:]')"
  CONTENT="${CONTENT%.}"
}

have
[[ $COUNT == 0 || ($COUNT == 1 && $CONTENT == "$LAB_HOST") ]] ||
  die "$NAME already has a record that is not the rehearsal's (it leads elsewhere than $LAB_HOST): it is left alone. Pick another --name"

if [[ $MODE == apply ]]; then
  if [[ $WANT == present && $COUNT == 0 ]]; then
    out="$(pb -X POST -d "$(record_body)" "$PORKBUN_API/dns/create/$REHEARSAL_DOMAIN")" || die "Porkbun gave no answer"
    pb_ok "$out" || die "Porkbun did not make the record: $(printf '%s' "$out" | jq -c '{status, message}' 2>/dev/null || echo "no JSON in the answer")"
    say "set      the record of $NAME: a CNAME of $LAB_HOST, for $TTL s"
  elif [[ $WANT == absent && $COUNT == 1 ]]; then
    out="$(pb -X POST -d '{}' "$PORKBUN_API/dns/deleteByNameType/$REHEARSAL_DOMAIN/CNAME/$SUB")" || die "Porkbun gave no answer"
    pb_ok "$out" || die "Porkbun did not delete the record: $(printf '%s' "$out" | jq -c '{status, message}' 2>/dev/null || echo "no JSON in the answer")"
    say "set      the record of $NAME: deleted"
  fi
  have
fi

if [[ $WANT == absent ]]; then
  if [[ $COUNT == 0 ]]; then
    say "ok       the record of $NAME: there is none"
  else
    drifted "the record of $NAME" "is still there"
  fi
elif [[ $COUNT == 0 ]]; then
  drifted "the record of $NAME" "there is none"
else
  say "ok       the record of $NAME: a CNAME of $LAB_HOST"
fi
if (( ${#DRIFTED[@]} )); then
  say "$ME: not as it should be: ${DRIFTED[0]}"
  exit 1
fi
say "$ME: the record of $NAME is as it should be"
