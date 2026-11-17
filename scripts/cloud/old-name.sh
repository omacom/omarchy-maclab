#!/usr/bin/env bash
# The old site's name, omarchy-m-testing.org, on Cloudflare (the Cutover's
# steps 19 and 25; spec section 16, "The old world's end"). Nothing here is
# applied before the day.
#
#   old-name.sh zone [--check | --apply]      step 19: the zone, in the same account, with
#                                             the same records the registrar serves today,
#                                             still pointing at Railway
#   old-name.sh redirect [--check | --apply]  step 25: the one 308 rule, apex and www, to
#                                             the same path and query at the Lab address
#   old-name.sh dnssec --check                DNSSEC is active on the zone (the owner turns
#                                             it on once the zone is active)
#
# With neither option: print the API calls; send nothing. --check only
# reads and exits 1 when something is not as it should be: for zone, that
# Cloudflare shows the zone active and its certificate issued, which is
# what the Freeze (step 20) waits for.
#
#   --token-file FILE  the Cloudflare API token, in a file only you can read;
#                      without it the script asks, and does not show it
#
# zone --apply reads the records at Porkbun, whose two keys come from
# --porkbun-keys FILE (see lib.sh), and needs CLOUDFLARE_ACCOUNT_ID in
# lab.env to make the zone. It moves no nameserver: that is nameservers.sh,
# started by the owner with DOMAIN=omarchy-m-testing.org. The address
# records of the apex and www are proxied, so that the certificate is
# Cloudflare's and the redirect rule can answer; every other record is as
# it was. The redirect is a rule and nothing else: no Worker, and the lab
# never answers at this name.

# shellcheck source=scripts/cloud/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
parse_mode "$@"
[[ ${#ARGS[@]} == 1 ]] || usage 2 30
WHAT="${ARGS[0]}"
case "$WHAT" in zone | redirect | dnssec) ;; *) usage 2 30 ;; esac

WWW="www.$OLD_DOMAIN"
PHASE="/zones/{zone}/rulesets/phases/http_request_dynamic_redirect/entrypoint"
RULE="$(jq -nc --arg old "$OLD_DOMAIN" --arg www "$WWW" --arg lab "$LAB_HOST" '{
  description: "\($old) is now \($lab): everything, path and query kept",
  expression: "(http.host eq \"\($old)\" or http.host eq \"\($www)\")",
  action: "redirect",
  action_parameters: {from_value: {status_code: 308, target_url: {expression: "concat(\"https://\($lab)\", http.request.uri.path)"}, preserve_query_string: true}},
  enabled: true}')"

new_zone() { jq -nc --arg name "$OLD_DOMAIN" --arg account "$1" '{name: $name, type: "full", account: {id: $account}}'; }

# as_cloudflare: Porkbun's records on stdin, as the bodies Cloudflare takes,
# a line each. The registrar's own NS records stay behind; an ALIAS is a
# CNAME, which Cloudflare flattens at the apex.
as_cloudflare() {
  jq -c --arg old "$OLD_DOMAIN" --arg www "$WWW" '.records[]?
    | select((.type == "NS" and .name == $old) | not)
    | (if .type == "ALIAS" then "CNAME" else .type end) as $type
    | {type: $type, name, content, ttl: 1}
      + (if (.prio // "") != "" and ($type == "MX" or $type == "SRV") then {priority: (.prio | tonumber)} else {} end)
      + (if ($type == "A" or $type == "AAAA" or $type == "CNAME") then {proxied: (.name == $old or .name == $www)} else {} end)'
}

if [[ $MODE == dry-run ]]; then
  say "$ME: a dry run. These are the calls --apply makes; nothing is sent."
  say
  case "$WHAT" in
    zone)
      say "the zone $OLD_DOMAIN, in the lab's account"
      would "GET /zones?name=$OLD_DOMAIN"
      would "POST /zones $(new_zone "<CLOUDFLARE_ACCOUNT_ID>")"
      say "the records the registrar serves today, from Porkbun"
      would "POST $PORKBUN_API/dns/retrieve/$OLD_DOMAIN {}"
      say "each of them, when the zone lacks it; the apex and www proxied"
      would "GET /zones/{zone}/dns_records?per_page=500"
      would "POST /zones/{zone}/dns_records {\"type\":\"<TYPE>\",\"name\":\"<NAME>\",\"content\":\"<CONTENT>\",\"ttl\":1}"
      say "checked: the zone is active and its certificate issued"
      would "GET /zones/{zone}/ssl/certificate_packs?status=active"
      ;;
    redirect)
      say "the one redirect rule of $OLD_DOMAIN"
      would "GET /zones?name=$OLD_DOMAIN"
      would "GET $PHASE"
      would "PUT $PHASE {\"rules\":[$RULE]}"
      ;;
    dnssec)
      say "checked, the owner's to set: DNSSEC on $OLD_DOMAIN"
      would "GET /zones?name=$OLD_DOMAIN"
      would "GET /zones/{zone}/dnssec"
      ;;
  esac
  exit 0
fi

[[ $WHAT != dnssec || $MODE == check ]] || die "DNSSEC is the owner's to turn on (Cloudflare: DNS > Settings > DNSSEC > Enable, then the DS record it shows goes in at Porkbun): this only checks it"

load_token
find_zone "$OLD_DOMAIN"

zone() {
  local out have want line n=0 held status packs
  if [[ -z $ZONE_ID ]]; then
    [[ $MODE == apply ]] || die "the account has no zone $OLD_DOMAIN: old-name.sh zone --apply makes it (step 19)"
    [[ -n ${CLOUDFLARE_ACCOUNT_ID:-} ]] || die "CLOUDFLARE_ACCOUNT_ID is not set in $SCRIPTS_DIR/lab.env: see lab.env.example"
    out="$(cf POST /zones "$(new_zone "$CLOUDFLARE_ACCOUNT_ID")")" || exit 1
    ZONE_ID="$(printf '%s' "$out" | jq -r '.result.id')"
    ZONE="$(printf '%s' "$out" | jq -c '.result')"
    say "made     the zone $OLD_DOMAIN"
  fi
  if [[ $MODE == apply ]]; then
    held="$(pb -X POST -d '{}' "$PORKBUN_API/dns/retrieve/$OLD_DOMAIN")" || exit 1
    pb_ok "$held" || die "Porkbun did not give $OLD_DOMAIN's records: $(printf '%s' "$held" | jq -c '{code, message}' 2>/dev/null || echo "no answer it could read")"
    want="$(printf '%s' "$held" | as_cloudflare)"
    [[ -n $want ]] || die "Porkbun holds no record for $OLD_DOMAIN: nothing would point at Railway. Nothing was added"
    out="$(cf GET "/zones/{zone}/dns_records?per_page=500")" || exit 1
    have="$(printf '%s' "$out" | jq -c '[.result[]? | {type, name, content}]')"
    while IFS= read -r line; do
      if printf '%s' "$have" | jq -e --argjson r "$line" 'any(.[]; .type == $r.type and .name == $r.name and .content == $r.content)' >/dev/null; then
        continue
      fi
      cf POST "/zones/{zone}/dns_records" "$line" >/dev/null || exit 1
      n=$((n + 1))
      say "added    $(printf '%s' "$line" | jq -r '"\(.type) \(.name)\(if .proxied then ", proxied" else "" end)"')"
    done <<<"$want"
    say "$ME: $n record(s) added; the zone holds what Porkbun serves for $OLD_DOMAIN"
    say "$ME: its nameservers, for Porkbun: $(printf '%s' "$ZONE" | jq -r '(.name_servers // []) | join(" ")'). You change them: DOMAIN=$OLD_DOMAIN $SCRIPTS_DIR/nameservers.sh --apply"
  fi
  status="$(printf '%s' "$ZONE" | jq -r '.status // "unknown"')"
  out="$(cf GET "/zones/{zone}/ssl/certificate_packs?status=active")" || exit 1
  packs="$(printf '%s' "$out" | jq --arg old "$OLD_DOMAIN" '[.result[]? | select(.status == "active" and ((.hosts // []) | index($old) != null))] | length')"
  if [[ $status == active && $packs -gt 0 ]]; then
    say "$ME: Cloudflare shows $OLD_DOMAIN active and its certificate issued"
    return 0
  fi
  [[ $status == active ]] || say "not yet  the zone is $status, not active: Cloudflare has not seen its nameservers at the registrar"
  [[ $packs -gt 0 ]] || say "not yet  no certificate is issued for $OLD_DOMAIN"
  return 1
}

# redirect_is: same, or what differs.
redirect_is() {
  local out rc=0
  out="$(cf GET "$PHASE")" || rc=$?
  case "$rc" in
    0) ;;
    4) out='{"result":{"rules":[]}}' ;;
    *) exit 1 ;;
  esac
  printf '%s' "$out" | jq -r --argjson want "$RULE" '(.result.rules // []) as $have
    | if ($have | length) == 0 then "no redirect rule"
      elif ($have | length) != 1 then "\($have | length) redirect rules, want the one"
      elif ($have[0] | {expression, action, action_parameters, enabled}) != ($want | {expression, action, action_parameters, enabled}) then "the redirect rule is not as written"
      else "same" end'
}

redirect() {
  local is
  [[ -n $ZONE_ID ]] || die "the account has no zone $OLD_DOMAIN: old-name.sh zone --apply makes it (step 19)"
  is="$(redirect_is)"
  if [[ $is != same && $MODE == apply ]]; then
    cf PUT "$PHASE" "{\"rules\":[$RULE]}" >/dev/null || exit 1
    say "set      the redirect rule"
    is="$(redirect_is)"
  fi
  if [[ $is == same ]]; then
    say "$ME: $OLD_DOMAIN and $WWW answer 308 to the same path and query at https://$LAB_HOST"
    return 0
  fi
  say "drifted  $is"
  return 1
}

dnssec() {
  local out is
  [[ -n $ZONE_ID ]] || die "the account has no zone $OLD_DOMAIN"
  out="$(cf GET "/zones/{zone}/dnssec")" || exit 1
  is="$(printf '%s' "$out" | jq -r '.result.status // "unknown"')"
  if [[ $is == active ]]; then
    say "$ME: DNSSEC is active on $OLD_DOMAIN"
    return 0
  fi
  say "not yet  DNSSEC is $is on $OLD_DOMAIN. The owner sets it: Cloudflare: DNS > Settings > DNSSEC > Enable, then the DS record it shows goes in at Porkbun"
  return 1
}

"$WHAT"
