#!/usr/bin/env bash
# Point $DOMAIN (omarchy-mac.org unless DOMAIN is set) at the pair of
# nameservers Cloudflare gave its zone.
#
#   nameservers.sh            print the API calls; send nothing
#   nameservers.sh --check    compare the registrar's set with the zone's pair; exit 1 when they differ
#   nameservers.sh --apply    change the registrar's set when they differ
#
#   --token-file FILE  the Cloudflare API token, in a file only you can read;
#                      without it the script asks, and does not show it
#
# Porkbun's two keys come from --porkbun-keys FILE (see lib.sh).

# shellcheck source=scripts/cloud/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
parse_mode "$@"
[[ ${#ARGS[@]} == 0 ]] || usage 2 12

if [[ $MODE == dry-run ]]; then
  say "$ME: a dry run. These are the calls --apply makes; nothing is sent."
  say
  say "the zone's pair, from Cloudflare"
  would "GET /zones?name=$DOMAIN"
  say "the registrar's set, from Porkbun"
  would "POST $PORKBUN_API/domain/listAll {}"
  would "POST $PORKBUN_API/dns/getDnssecRecords/$DOMAIN {}"
  would "POST $PORKBUN_API/domain/getNs/$DOMAIN {}"
  would "POST $PORKBUN_API/domain/updateNs/$DOMAIN {\"ns\":[<the zone's pair>]}"
  exit 0
fi

load_token
find_zone "$DOMAIN"
[[ -n $ZONE_ID ]] || die "no Cloudflare zone for $DOMAIN: cloudflare-zone.sh --apply makes it"
want="$(printf '%s' "$ZONE" | jq -c '.name_servers | map(ascii_downcase) | sort')"

held="$(pb_domain)"
[[ -n $held ]] || die "$DOMAIN is not in the Porkbun account"
[[ "$(printf '%s' "$held" | jq -r '.apiAccess')" == 1 ]] ||
  die "API access is off for $DOMAIN: the owner turns it on in the domain's details at https://porkbun.com/account/domainsSpeedy"

# Cloudflare needs DNSSEC off at the registrar before the nameservers change.
ds="$(pb -X POST -d '{}' "$PORKBUN_API/dns/getDnssecRecords/$DOMAIN")"
pb_ok "$ds" || die "Porkbun's DNSSEC lookup failed: $(printf '%s' "$ds" | jq -c '{code, message}')"
[[ "$(printf '%s' "$ds" | jq -r '.records | length')" == 0 ]] || die "DS records exist at the registry: remove them first"

ns="$(pb -X POST -d '{}' "$PORKBUN_API/domain/getNs/$DOMAIN")"
pb_ok "$ns" || die "Porkbun's nameserver lookup failed: $(printf '%s' "$ns" | jq -c '{code, message}')"
have="$(printf '%s' "$ns" | jq -c '.ns | map(ascii_downcase) | sort')"

say "registrar: $have"
say "zone:      $want"
if [[ $have == "$want" ]]; then
  say "$ME: $DOMAIN is delegated to the zone's pair"
  exit 0
fi
[[ $MODE == apply ]] || die "the registrar's nameservers are not the zone's pair"

out="$(pb -X POST -d "$(jq -nc --argjson ns "$want" '{ns: $ns}')" "$PORKBUN_API/domain/updateNs/$DOMAIN")"
pb_ok "$out" || die "Porkbun's nameserver update failed: $(printf '%s' "$out" | jq -c '{code, message}')"
say "$ME: the registrar's nameservers are now the zone's pair"
