#!/usr/bin/env bash
# The zone script: every edge setting of the Lab address's zone, set and
# checked from edge.json beside it.
#
#   zone-settings.sh                              print the API calls; send nothing
#   zone-settings.sh --check                      read everything; exit 1 and name what drifted
#   zone-settings.sh --apply --forward-to MAILBOX set what drifted, then check
#
#   --token-file FILE     the Cloudflare API token, in a file only you can
#                         read; without it the script asks, and does not show it
#   --forward-to MAILBOX  where privacy@omarchy-mac.org is forwarded. --apply
#                         needs it; with --check the rule must name it
#
# Set and checked: each setting that is off or on, the one skip rule, the one
# rate rule, the cache rules, that there is no redirect rule, the mail
# forwarding rule. Checked and never set, because they are the owner's:
# DNSSEC and the two tunnel alerts. The Alias (its fallback origin and its
# custom hostname) is made and checked once edge.json says alias.wanted,
# which it does from the day the other side says yes; alias.sh is the same
# calls for that one thing.

# shellcheck source=scripts/cloud/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
parse_mode "$@"
set -- ${ARGS[@]+"${ARGS[@]}"}

FORWARD_TO=""
while (( $# )); do
  case "$1" in
    --forward-to)
      [[ $# -ge 2 ]] || usage 2 20
      FORWARD_TO="$2"
      shift
      ;;
    *) usage 2 20 ;;
  esac
  shift
done

EDGE="$SCRIPTS_DIR/edge.json"
MAIL="$(jq -r '.mail.address' "$EDGE")"
MAIL_NAME="$(jq -r '.mail.name' "$EDGE")"
DRIFTED=()
SENT=""
GOT=""
GOT_PATH=""

drifted() {
  DRIFTED+=("$1")
  say "drifted  $1: $2"
}

# get PATH: the answer to GET PATH, in GOT. The last one is kept.
get() {
  if [[ $1 != "$GOT_PATH" ]]; then
    GOT="$(cf GET "$1")" || exit 1
    GOT_PATH="$1"
  fi
}

# send METHOD PATH BODY: one write, once a run.
send() {
  local call="$1 $2 $3"
  GOT_PATH=""
  case "$SENT" in *"$call"$'\n'*) return 0 ;; esac
  cf "$1" "$2" "$3" >/dev/null || exit 1
  SENT+="$call"$'\n'
}

# rows KEY: edge.json's list KEY, an entry a line, separated by tabs: name,
# state, path, how to read it, the values accepted, method, body.
rows() {
  jq -r --arg key "$1" '.[$key][] | [
      .name, (.state // "-"), .path, (.read // ".result.value"),
      ((.accept // [.state]) | tojson),
      (.method // (if .how then "-" else "PATCH" end)),
      ((.body // {value: .state}) | tojson)
    ] | @tsv' "$EDGE"
}

# settings KEY fix|judge: each entry of KEY read; one that is not as wanted
# is set (fix) or named (judge). An entry with no method is the owner's.
settings() {
  local name state path read accept method body is want good
  while IFS=$'\t' read -r name state path read accept method body <&3; do
    get "$path"
    IFS=$'\t' read -r is want good < <(printf '%s' "$GOT" |
      jq -r --argjson accept "$accept" "($read | tostring) as \$is | [\$is, \$accept[0], (\$accept | index(\$is) != null)] | @tsv")
    if [[ $good == true ]]; then
      [[ $2 != judge ]] || say "ok       $name: $is"
    elif [[ $method == - ]]; then
      [[ $2 != judge ]] || drifted "$name" "is $is, want $want. The owner sets it: $(jq -r --arg n "$name" '.owner[] | select(.name == $n) | .how' "$EDGE")"
    elif [[ $2 == fix ]]; then
      send "$method" "$path" "$body"
      say "set      $name: $state"
    else
      drifted "$name" "is $is, want $want"
    fi
  done 3< <(rows "$1")
}

# What rules a phase has against what it should have: prints "same", or what
# differs. A rule matches when it has everything written here; Cloudflare
# adds fields of its own (ids, versions, defaults). A list of plain values is
# compared as a set, any other list in order.
# shellcheck disable=SC2016
RULES_JQ='
def plain: all(.[]; type != "object" and type != "array");
def covers($w; $h):
  if ($w | type) == "object" then
    ($h | type) == "object" and all($w | keys[]; . as $k | covers($w[$k]; $h[$k]))
  elif ($w | type) == "array" then
    ($h | type) == "array" and ($w | length) == ($h | length) and
    (if ($w | plain) and ($h | plain) then ($w | sort) == ($h | sort)
     else all(range(0; $w | length); . as $i | covers($w[$i]; $h[$i])) end)
  else $w == $h end;
(.result.rules // []) as $have
| [ $have[] | (.action_parameters // {}) | keys[] | select(. as $k | $never | index($k) != null) ] as $forced
| if ($forced | length) > 0 then "a rule sets \($forced | unique | join(" and "))"
  elif ($have | length) != ($want | length) then "has \($have | length) rules, want \($want | length)"
  else ([range(0; $want | length) | select(covers($want[.]; $have[.]) | not) | . + 1]) as $off
    | if ($off | length) > 0 then "rule \($off | map(tostring) | join(" and ")) is not as written" else "same" end
  end'

# rules fix|judge
rules() {
  local n i name phase want never out rc verdict path
  n="$(jq '.rules | length' "$EDGE")"
  for (( i = 0; i < n; i++ )); do
    {
      read -r name
      read -r phase
      read -r want
      read -r never
    } < <(jq -r ".rules[$i] | .name, .phase, (.want | tojson), ((.never // []) | tojson)" "$EDGE")
    path="/zones/{zone}/rulesets/phases/$phase/entrypoint"
    rc=0
    out="$(cf GET "$path")" || rc=$?
    case "$rc" in
      0) ;;
      4) out='{"result":{"rules":[]}}' ;;
      *) exit 1 ;;
    esac
    verdict="$(printf '%s' "$out" | jq -r --argjson want "$want" --argjson never "$never" "$RULES_JQ")"
    if [[ $verdict == same ]]; then
      [[ $1 != judge ]] || say "ok       $name"
    elif [[ $1 == fix ]]; then
      send PUT "$path" "{\"rules\":$want}"
      say "set      $name"
    else
      drifted "$name" "$verdict"
    fi
  done
}

same_mailbox() { [[ "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" == "$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')" ]]; }

# mail fix|judge: privacy@ is forwarded to one mailbox that has confirmed
# Cloudflare's mail. The mailbox is never printed.
mail() {
  local name="the mail forwarding rule" out addresses routing found count rule_id to given want
  out="$(cf GET "/accounts/{account}/email/routing/addresses?per_page=50")" || exit 1
  addresses="$(printf '%s' "$out" | jq -c '[.result[]? | {email: (.email | ascii_downcase), verified: (.verified != null)}]')"
  out="$(cf GET "/zones/{zone}/email/routing")" || exit 1
  routing="$(printf '%s' "$out" | jq -r '.result.enabled == true')"
  out="$(cf GET "/zones/{zone}/email/routing/rules?per_page=50")" || exit 1
  found="$(printf '%s' "$out" | jq -c --arg to "$MAIL" '[.result[]? | select(any(.matchers[]?; .type == "literal" and .field == "to" and .value == $to))]')"
  count="$(printf '%s' "$found" | jq 'length')"
  rule_id="$(printf '%s' "$found" | jq -r '.[0].id // .[0].tag // empty')"
  to="$(printf '%s' "$found" | jq -r 'if length == 1 and .[0].enabled == true and (.[0].actions | length) == 1 and .[0].actions[0].type == "forward" and (.[0].actions[0].value | length) == 1 then .[0].actions[0].value[0] else empty end')"

  given=""
  if [[ -n $FORWARD_TO ]]; then
    given="$(printf '%s' "$addresses" | jq -r --arg m "$(printf '%s' "$FORWARD_TO" | tr '[:upper:]' '[:lower:]')" '[.[] | select(.email == $m)] | if length == 0 then "missing" elif .[0].verified then "confirmed" else "waiting" end')"
  fi

  if [[ $1 == fix ]]; then
    if [[ $routing != true ]]; then
      send POST "/zones/{zone}/email/routing/dns" "{}"
      say "set      $name: mail routing is on for $LAB_DOMAIN"
    fi
    if [[ $given == missing ]]; then
      send POST "/accounts/{account}/email/routing/addresses" "$(jq -nc --arg m "$FORWARD_TO" '{email: $m}')"
      say "set      $name: Cloudflare has mailed the mailbox a link to confirm"
    fi
    # Cloudflare takes a rule only for a mailbox that has confirmed.
    if [[ $given != confirmed || $count -gt 1 ]]; then
      return 0
    fi
    if [[ -z $to ]] || ! same_mailbox "$to" "$FORWARD_TO"; then
      want="$(jq -nc --arg n "$MAIL_NAME" --arg to "$MAIL" --arg m "$FORWARD_TO" '{name: $n, enabled: true, matchers: [{type: "literal", field: "to", value: $to}], actions: [{type: "forward", value: [$m]}]}')"
      if [[ -n $rule_id ]]; then
        send PUT "/zones/{zone}/email/routing/rules/$rule_id" "$want"
      else
        send POST "/zones/{zone}/email/routing/rules" "$want"
      fi
      say "set      $name: $MAIL goes to the mailbox given"
    fi
    return 0
  fi

  if [[ $given == missing || $given == waiting ]]; then
    drifted "$name" "the mailbox given has not confirmed Cloudflare's mail yet: open the link in it, then run this again"
    return 0
  fi
  if [[ $routing != true ]]; then
    drifted "$name" "mail routing is off for $LAB_DOMAIN"
  elif [[ $count == 0 ]]; then
    drifted "$name" "no rule forwards $MAIL"
  elif [[ $count -gt 1 ]]; then
    drifted "$name" "$count rules match $MAIL, want one: remove the others in the dashboard"
  elif [[ -z $to ]]; then
    drifted "$name" "the rule for $MAIL is off, or does not forward to one mailbox"
  elif [[ -n $FORWARD_TO ]] && ! same_mailbox "$to" "$FORWARD_TO"; then
    drifted "$name" "$MAIL goes to another mailbox than the one given"
  elif [[ "$(printf '%s' "$addresses" | jq -r --arg m "$(printf '%s' "$to" | tr '[:upper:]' '[:lower:]')" 'any(.[]; .email == $m and .verified)')" != true ]]; then
    drifted "$name" "the mailbox has not confirmed Cloudflare's mail yet: open the link in it, then run this again"
  else
    say "ok       $name: $MAIL goes to a confirmed mailbox"
  fi
}

# the_alias fix|judge: nothing of the Alias is read before it is wanted,
# since Cloudflare takes none of its calls until the owner has entered
# payment details.
the_alias() {
  if [[ $ALIAS_WANTED != true ]]; then
    [[ $1 != judge ]] || say "ok       the Alias: not made yet (edge.json says alias.wanted is false), so nothing of it is read. alias.sh makes it the day the other side says yes"
    return 0
  fi
  alias_origin "$1"
  alias_hostname "$1" "$ALIAS_HOST" present
}

plan() {
  local name state path read accept method body n i phase want
  say "$ME: a dry run. These are the calls --apply makes; nothing is sent."
  say "A write is sent only for what has drifted."
  say
  say "the zone"
  would "GET /zones?name=$LAB_DOMAIN"
  while IFS=$'\t' read -r name state path read accept method body <&3; do
    say "$state: $name"
    would "GET $path"
    would "$method $path $body"
  done 3< <(rows settings)
  n="$(jq '.rules | length' "$EDGE")"
  for (( i = 0; i < n; i++ )); do
    name="$(jq -r ".rules[$i].name" "$EDGE")"
    phase="$(jq -r ".rules[$i].phase" "$EDGE")"
    want="$(jq -c ".rules[$i].want" "$EDGE")"
    say "$name"
    would "GET /zones/{zone}/rulesets/phases/$phase/entrypoint"
    would "PUT /zones/{zone}/rulesets/phases/$phase/entrypoint {\"rules\":$want}"
  done
  say "the mail forwarding rule: $MAIL to the mailbox of --forward-to"
  would "GET /accounts/{account}/email/routing/addresses?per_page=50"
  would "POST /accounts/{account}/email/routing/addresses {\"email\":\"<MAILBOX>\"}"
  would "GET /zones/{zone}/email/routing"
  would "POST /zones/{zone}/email/routing/dns {}"
  would "GET /zones/{zone}/email/routing/rules?per_page=50"
  would "POST /zones/{zone}/email/routing/rules $(jq -nc --arg n "$MAIL_NAME" --arg to "$MAIL" '{name: $n, enabled: true, matchers: [{type: "literal", field: "to", value: $to}], actions: [{type: "forward", value: ["<MAILBOX>"]}]}')"
  say "the Alias, once edge.json says alias.wanted (now: $ALIAS_WANTED)"
  alias_plan "$ALIAS_HOST" present
  while IFS=$'\t' read -r name state path read accept method body <&3; do
    say "checked, the owner's to set: $name"
    would "GET $path"
  done 3< <(rows owner)
}

pass() {
  GOT_PATH=""
  settings settings "$1"
  rules "$1"
  mail "$1"
  the_alias "$1"
  settings owner "$1"
}

if [[ $MODE == dry-run ]]; then
  plan
  exit 0
fi
[[ $MODE != apply || -n $FORWARD_TO ]] || die "--apply needs --forward-to MAILBOX: where $MAIL is forwarded"
[[ -z $FORWARD_TO || $FORWARD_TO == *@*.* ]] || die "--forward-to takes a mailbox, such as name@example.org"
load_token
find_zone "$LAB_DOMAIN"
[[ -n $ZONE_ID ]] || die "the account has no zone $LAB_DOMAIN: cloudflare-zone.sh makes it"
if [[ $MODE == apply ]]; then
  pass fix
fi
pass judge
if (( ${#DRIFTED[@]} )); then
  list="$(printf '%s; ' "${DRIFTED[@]}")"
  say "$ME: ${#DRIFTED[@]} not as the spec has it: ${list%; }"
  exit 1
fi
say "$ME: every edge setting of $LAB_DOMAIN is as the spec has it"
