#!/usr/bin/env bash
# What the cloud scripts share: the names, the three ways a script runs and
# how Cloudflare and Porkbun are asked. Source it; do not run it.
#
# Every script here runs one of three ways:
#
#   --dry-run          the default: print the API calls it would make, send nothing
#   --check            read only; exit 1 when something is not as it should be
#   --apply            change what is not as it should be, then check
#
#   --token-file FILE  the Cloudflare API token, in a file only you can read.
#                      Without it the script asks for the token and does not
#                      show it. The token is never printed, never an argument
#                      of a command and never written anywhere.
#
#   --porkbun-keys FILE  for the scripts that ask Porkbun: its API key on the
#                      first line and its secret key on the second, in a file
#                      only you can read (chmod 600). Without it,
#                      ~/.config/omarchy-mac-lab/porkbun.keys. The keys are
#                      read from nowhere else: not the environment, not the
#                      Keychain.
#
# Needs bash, curl 7.84 or later and jq: what macOS has.
# shellcheck disable=SC2034

set -euo pipefail
set +x

SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ME="$(basename "$0")"
LAB_DOMAIN="omarchy-mac.org"
LAB_HOST="lab.$LAB_DOMAIN"
# The old site's name: from the lab's opening a redirect and nothing else.
OLD_DOMAIN="omarchy-m-testing.org"
DOMAIN="${DOMAIN:-$LAB_DOMAIN}"
USER_AGENT="omarchy-mac-lab-scripts"
PORKBUN_API="https://api.porkbun.com/api/json/v3"
CLOUDFLARE_API="https://api.cloudflare.com/client/v4"

die() { printf '%s: %s\n' "$ME" "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
# would CALL...: one line of a dry run.
would() { printf '  %s\n' "$*"; }

# The tests stand a server on this machine in for Cloudflare's API.
if [[ -n ${LAB_CLOUD_API:-} ]]; then
  [[ $LAB_CLOUD_API =~ ^http://127\.0\.0\.1:[0-9]+(/.*)?$ ]] || die "LAB_CLOUD_API is the tests' and takes a loopback address only"
  CLOUDFLARE_API="$LAB_CLOUD_API"
fi

# And for Porkbun's, with made-up keys in a file (--porkbun-keys).
if [[ -n ${LAB_PORKBUN_API:-} ]]; then
  [[ $LAB_PORKBUN_API =~ ^http://127\.0\.0\.1:[0-9]+(/.*)?$ ]] || die "LAB_PORKBUN_API is the tests' and takes a loopback address only"
  PORKBUN_API="$LAB_PORKBUN_API"
fi

# Names with no value that is a secret (CLOUDFLARE_ACCOUNT_ID) may sit beside
# the scripts in lab.env, which is not committed: see lab.env.example.
if [[ -f "$SCRIPTS_DIR/lab.env" ]]; then
  # shellcheck disable=SC1091
  source "$SCRIPTS_DIR/lab.env"
fi

MODE=dry-run
TOKEN_FILE=""
PORKBUN_KEYS="${HOME:-}/.config/omarchy-mac-lab/porkbun.keys"
ARGS=()

# parse_mode "$@": takes the options every script has and leaves the rest in
# ARGS. Two different modes are refused: which one was meant is not guessed.
parse_mode() {
  local given=""
  ARGS=()
  while (( $# )); do
    case "$1" in
      --dry-run | --check | --apply)
        [[ -z $given || $given == "$1" ]] || die "$given and $1 are two modes: give one. Nothing was done."
        given="$1"
        MODE="${1#--}"
        ;;
      --token-file)
        [[ $# -ge 2 ]] || die "--token-file needs a file"
        TOKEN_FILE="$2"
        shift
        ;;
      --porkbun-keys)
        [[ $# -ge 2 ]] || die "--porkbun-keys needs a file"
        PORKBUN_KEYS="$2"
        shift
        ;;
      *) ARGS+=("$1") ;;
    esac
    shift
  done
}

# usage FIRST LAST: the script's own header, from line FIRST to line LAST.
usage() {
  sed -n "$1,$2p" "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

TOKEN=""

# own_only FILE: dies unless only its owner may read FILE: mode 600 or 400,
# and no ACL. GNU ls marks an ACL with +; macOS marks any extended attribute
# with @ and then shows no +, and lists the ACL itself under ls -e.
own_only() {
  local listed
  # shellcheck disable=SC2012
  case "$(ls -lnLd "$1" | awk '{ print $1; exit }')" in
    -r-------- | -rw------- | -r--------[@.] | -rw-------[@.]) ;;
    *+) die "$1 has an ACL, which may let others read it: take it off (chmod -N $1, or setfacl -b $1)" ;;
    *) die "$1 can be read by others: chmod 600 it" ;;
  esac
  # shellcheck disable=SC2012
  if listed="$(ls -lnLde "$1" 2>/dev/null)" && [[ $listed == *$'\n'* ]]; then
    die "$1 has an ACL, which may let others read it: take it off (chmod -N $1)"
  fi
}

# load_token: the Cloudflare token from --token-file, or asked for at the
# terminal. Call it before the first cf, outside any $(...).
load_token() {
  [[ -z $TOKEN ]] || return 0
  if [[ -n $TOKEN_FILE ]]; then
    [[ -f $TOKEN_FILE ]] || die "no token file $TOKEN_FILE"
    own_only "$TOKEN_FILE"
    IFS= read -r TOKEN < "$TOKEN_FILE" || true
  elif [[ -t 0 ]]; then
    printf 'Cloudflare API token (not shown): ' >&2
    IFS= read -rs TOKEN || true
    printf '\n' >&2
  else
    die "no token: give --token-file FILE, or run it at a terminal and be asked"
  fi
  TOKEN="${TOKEN//[[:space:]]/}"
  [[ -n $TOKEN ]] || die "the token is empty"
}

ZONE_ID=""
ACCOUNT_ID=""

# cf METHOD PATH [BODY]: one call to Cloudflare's API; {zone} and {account}
# in PATH are filled in. The answer's JSON goes to stdout. Exit 0 when
# Cloudflare says success, 4 when there is no such thing (404), 1 otherwise,
# with the status, Cloudflare's own error lines and the ray id on stderr and
# never the answer's body. The token reaches curl through a pipe.
cf() {
  local method="$1" path="$2" body="${3:-}" url out last status ray answer lines
  [[ -n $TOKEN ]] || die "cf before load_token"
  url="${2//\{zone\}/$ZONE_ID}"
  url="$CLOUDFLARE_API${url//\{account\}/$ACCOUNT_ID}"
  if [[ -n $body ]]; then
    out="$(printf '%s' "$body" | curl -sS -m 60 -A "$USER_AGENT" -X "$method" \
      -H @<(printf 'Authorization: Bearer %s\n' "$TOKEN") -H 'Content-Type: application/json' \
      --data-binary @- -w '\n%{http_code} %header{cf-ray}' "$url")" || die "$method $path: no answer"
  else
    out="$(curl -sS -m 60 -A "$USER_AGENT" -X "$method" \
      -H @<(printf 'Authorization: Bearer %s\n' "$TOKEN") \
      -w '\n%{http_code} %header{cf-ray}' "$url")" || die "$method $path: no answer"
  fi
  last="${out##*$'\n'}"
  answer="${out%$'\n'*}"
  status="${last%% *}"
  ray="${last#* }"
  if [[ $status == 2* ]] && printf '%s' "$answer" | jq -e '.success == true' >/dev/null 2>&1; then
    printf '%s\n' "$answer"
    return 0
  fi
  [[ $status != 404 ]] || return 4
  lines="$(printf '%s' "$answer" | jq -r '[.errors[]? | "[\(.code)] \(.message)"] | .[0:5] | join("; ")' 2>/dev/null || true)"
  printf '%s: %s %s: status %s%s, ray %s\n' "$ME" "$method" "$path" "$status" "${lines:+, $lines}" "${ray:-none}" >&2
  return 1
}

# find_zone NAME: sets ZONE_ID and ACCOUNT_ID, or leaves both empty when the
# account has no such zone. Prints the zone.
find_zone() {
  local out
  out="$(cf GET "/zones?name=$1")" || exit 1
  ZONE_ID="$(printf '%s' "$out" | jq -r '.result[0].id // empty')"
  ACCOUNT_ID="$(printf '%s' "$out" | jq -r '.result[0].account.id // empty')"
  ZONE="$(printf '%s' "$out" | jq -c '.result[0] // empty')"
}

# The Alias (spec section 6): a custom hostname of the zone, which Cloudflare
# calls Cloudflare for SaaS, whose fallback origin is the Lab address. A
# request for the name then comes down the lab's tunnel with its own Host,
# and labd forwards it. The other side adds one record, a CNAME of the Lab
# address that is DNS only; the certificate is validated over HTTP, so
# nothing else is asked of them. The owner enters payment details on
# Cloudflare before any of these calls is taken. The script that sources
# this says what drifted with its own drifted NAME WHY.
ALIAS_HOST="$(jq -r '.alias.hostname' "$SCRIPTS_DIR/edge.json")"
ALIAS_ORIGIN="$(jq -r '.alias.fallback_origin' "$SCRIPTS_DIR/edge.json")"
ALIAS_WANTED="$(jq -r '.alias.wanted' "$SCRIPTS_DIR/edge.json")"

not_enabled() {
  say "$ME: Cloudflare did not take a call about custom hostnames. They need Cloudflare for SaaS on $LAB_DOMAIN, which the owner enables by entering payment details (SSL/TLS > Custom Hostnames > Enable), and a token with \"SSL and Certificates: Edit\" on the zone."
  exit 1
}

alias_hostname_body() { jq -nc --arg h "$1" '{hostname: $h, ssl: {method: "http", type: "dv"}}'; }

# alias_plan NAME present|absent: a dry run's lines.
alias_plan() {
  if [[ $2 == absent ]]; then
    say "the custom hostname $1: deleted when it is there. The fallback origin stays"
    would "GET /zones/{zone}/custom_hostnames?hostname=$1"
    would "DELETE /zones/{zone}/custom_hostnames/{id}"
    return 0
  fi
  say "the fallback origin: $ALIAS_ORIGIN, the Lab address, which the tunnel serves"
  would "GET /zones/{zone}/custom_hostnames/fallback_origin"
  would "PUT /zones/{zone}/custom_hostnames/fallback_origin {\"origin\":\"$ALIAS_ORIGIN\"}"
  say "the custom hostname $1: made when missing, its certificate validated over HTTP"
  would "GET /zones/{zone}/custom_hostnames?hostname=$1"
  would "POST /zones/{zone}/custom_hostnames $(alias_hostname_body "$1")"
}

# alias_origin fix|judge: the zone's fallback origin is the Lab address, and
# Cloudflare has it active.
alias_origin() {
  local rc=0 out origin status
  out="$(cf GET "/zones/{zone}/custom_hostnames/fallback_origin")" || rc=$?
  case "$rc" in
    0) ;;
    4) out='{"result":{}}' ;;
    *) not_enabled ;;
  esac
  origin="$(printf '%s' "$out" | jq -r '.result.origin // empty')"
  status="$(printf '%s' "$out" | jq -r '.result.status // empty')"
  if [[ $1 == fix ]]; then
    if [[ $origin != "$ALIAS_ORIGIN" ]]; then
      cf PUT "/zones/{zone}/custom_hostnames/fallback_origin" "$(jq -nc --arg o "$ALIAS_ORIGIN" '{origin: $o}')" >/dev/null || not_enabled
      say "set      the fallback origin: $ALIAS_ORIGIN"
    fi
    return 0
  fi
  if [[ -z $origin ]]; then
    drifted "the fallback origin" "there is none"
  elif [[ $origin != "$ALIAS_ORIGIN" ]]; then
    drifted "the fallback origin" "is $origin, want $ALIAS_ORIGIN"
  elif [[ $status == active ]]; then
    say "ok       the fallback origin: $origin, active"
  elif [[ $MODE == apply ]]; then
    say "ok       the fallback origin: $origin, not active yet (${status:-no state}): Cloudflare is still putting it in place"
  else
    drifted "the fallback origin" "is ${status:-in no state}, want active"
  fi
}

# alias_hostname fix|judge NAME present|absent: the zone's custom hostname
# NAME. Cloudflare's own filter gives every name that holds NAME, so the
# list is narrowed here. One that is there and not active is never made
# again: only the other side's record brings it back. judge prints one line
# of state that scripts/cutover/alias-rehearsal reads.
alias_hostname() {
  local name="$2" want="$3" out found count id status ssl method
  out="$(cf GET "/zones/{zone}/custom_hostnames?hostname=$name")" || not_enabled
  found="$(printf '%s' "$out" | jq -c --arg n "$name" '[.result[]? | select(.hostname == $n)]')"
  count="$(printf '%s' "$found" | jq 'length')"
  status="$(printf '%s' "$found" | jq -r '.[0].status // "none"')"
  ssl="$(printf '%s' "$found" | jq -r '.[0].ssl.status // "none"')"
  method="$(printf '%s' "$found" | jq -r '.[0].ssl.method // "none"')"
  if [[ $1 == fix ]]; then
    if [[ $want == present && $count == 0 ]]; then
      cf POST "/zones/{zone}/custom_hostnames" "$(alias_hostname_body "$name")" >/dev/null || not_enabled
      say "set      the custom hostname $name: made. It becomes active once $name is a CNAME of $ALIAS_ORIGIN, DNS only (not proxied), and nothing else is asked of the name's side"
    elif [[ $want == absent ]]; then
      while IFS= read -r id; do
        [[ $id =~ ^[0-9a-f-]{36}$ ]] || continue
        cf DELETE "/zones/{zone}/custom_hostnames/$id" >/dev/null || not_enabled
        say "set      the custom hostname $name: deleted"
      done < <(printf '%s' "$found" | jq -r '.[].id')
    fi
    return 0
  fi
  say "state    $name: hostname=$status certificate=$ssl method=$method"
  if [[ $want == absent ]]; then
    if [[ $count == 0 ]]; then
      say "ok       the custom hostname $name: there is none"
    else
      drifted "the custom hostname $name" "is still there ($status)"
    fi
  elif [[ $count == 0 ]]; then
    drifted "the custom hostname $name" "there is none"
  elif [[ $count -gt 1 ]]; then
    drifted "the custom hostname $name" "$count carry the name, want one: remove the others in the dashboard"
  elif [[ $method != http ]]; then
    drifted "the custom hostname $name" "its certificate is validated by $method, want http: nothing but the CNAME is asked of the name's side"
  elif [[ $status == active && $ssl == active ]]; then
    say "ok       the custom hostname $name: active, and so is its certificate, validated over HTTP"
  elif [[ $MODE == apply ]]; then
    say "ok       the custom hostname $name: not active yet (hostname $status, certificate $ssl). It waits for $name to be a CNAME of $ALIAS_ORIGIN, DNS only"
  else
    drifted "the custom hostname $name" "the hostname is $status and its certificate $ssl, want both active"
  fi
}

# Porkbun, the registrar. Its two keys come from --porkbun-keys FILE (or
# ~/.config/omarchy-mac-lab/porkbun.keys), a file only its owner reads, and
# reach curl as headers on stdin, never on a command line.
pb() {
  local key="" sec=""
  [[ -f $PORKBUN_KEYS ]] || die "no Porkbun keys file $PORKBUN_KEYS: its API key on the first line, its secret key on the second, chmod 600; or give --porkbun-keys FILE. Nothing was sent"
  own_only "$PORKBUN_KEYS"
  { IFS= read -r key || true; IFS= read -r sec || true; } < "$PORKBUN_KEYS"
  key="${key//[[:space:]]/}"
  sec="${sec//[[:space:]]/}"
  [[ -n $key && -n $sec ]] || die "$PORKBUN_KEYS holds no two keys: its API key on the first line, its secret key on the second. Nothing was sent"
  printf 'X-API-Key: %s\nX-Secret-API-Key: %s\nContent-Type: application/json\n' "$key" "$sec" |
    curl -sS -m 60 -A "$USER_AGENT" -H @- "$@"
}

pb_ok() { printf '%s' "$1" | jq -e '.status == "SUCCESS"' >/dev/null; }

# Prints the domain's Porkbun record, or nothing when the account does not hold it.
pb_domain() {
  local out
  out="$(pb -X POST -d '{}' "$PORKBUN_API/domain/listAll")" || exit 1
  pb_ok "$out" || die "Porkbun's domain list failed: $(printf '%s' "$out" | jq -c '{code, message}')"
  printf '%s' "$out" | jq -c --arg d "$DOMAIN" '.domains[]? | select(.domain == $d)'
}
