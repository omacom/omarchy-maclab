#!/usr/bin/env bash
# Register $DOMAIN (omarchy-mac.org unless DOMAIN is set) at Porkbun from
# account credit.
#
#   register-domain.sh                       print the API calls; send nothing
#   register-domain.sh --check               quote and rehearse; nothing is charged
#   register-domain.sh --apply --cost 798    buy, only if the live price is 798 cents
#
# A registration cannot be undone. --cost is the price the owner agreed to;
# the script refuses when the live price is another. Porkbun's two keys come
# from --porkbun-keys FILE (see lib.sh).

# shellcheck source=scripts/cloud/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
parse_mode "$@"
set -- ${ARGS[@]+"${ARGS[@]}"}
COST=""
while (( $# )); do
  case "$1" in
    --cost)
      [[ $# -ge 2 ]] || usage 2 11
      COST="$2"
      shift
      ;;
    *) usage 2 11 ;;
  esac
  shift
done

if [[ $MODE == dry-run ]]; then
  say "$ME: a dry run. These are the calls --apply makes; nothing is sent."
  say
  say "whether the account holds $DOMAIN already"
  would "POST $PORKBUN_API/domain/listAll {}"
  say "the price, and a rehearsal that charges nothing"
  would "POST $PORKBUN_API/domain/checkDomain/$DOMAIN {}"
  would "POST $PORKBUN_API/domain/create/$DOMAIN {\"cost\":0,\"agreeToTerms\":\"yes\",\"dryRun\":true}"
  say "the purchase, with --apply --cost and only at that price"
  would "POST $PORKBUN_API/domain/create/$DOMAIN {\"cost\":<--cost>,\"agreeToTerms\":\"yes\"}"
  exit 0
fi

held="$(pb_domain)"
if [[ -n $held ]]; then
  say "$ME: $DOMAIN is already in the Porkbun account; nothing to buy"
  printf '%s' "$held" | jq '{domain, status, createDate, expireDate, autoRenew, whoisPrivacy, apiAccess, securityLock}'
  exit 0
fi

check="$(pb -X POST -d '{}' "$PORKBUN_API/domain/checkDomain/$DOMAIN")"
pb_ok "$check" || die "Porkbun's availability check failed: $(printf '%s' "$check" | jq -c '{code, message}')"
printf '%s' "$check" | jq '.response | {avail, price, regularPrice, firstYearPromo, premium, minDuration, renewal: .additional.renewal.price}'
[[ "$(printf '%s' "$check" | jq -r '.response.avail')" == yes ]] || die "$DOMAIN is not available"
[[ "$(printf '%s' "$check" | jq -r '.response.premium')" == no ]] || die "$DOMAIN is a premium name; the API cannot register it"

rehearsal="$(pb -X POST -d '{"cost":0,"agreeToTerms":"yes","dryRun":true}' "$PORKBUN_API/domain/create/$DOMAIN")"
pb_ok "$rehearsal" || die "Porkbun's rehearsal failed: $(printf '%s' "$rehearsal" | jq -c '{code, message}')"
printf '%s' "$rehearsal" | jq '{wouldSucceed, duration, cost, costDisplay, balanceDisplay, sufficientFunds, withinMonthlySpendLimit, message}'
quote="$(printf '%s' "$rehearsal" | jq -r '.cost')"

if [[ $MODE != apply ]]; then
  say "To buy: $ME --apply --cost $quote"
  exit 0
fi

[[ -n $COST ]] || die "--apply needs --cost CENTS, the price the owner agreed to"
[[ $COST == "$quote" ]] || die "the live price is $quote cents, not the agreed $COST: ask the owner again"
[[ "$(printf '%s' "$rehearsal" | jq -r '.wouldSucceed')" == true ]] || die "the rehearsal says the purchase would fail: see its message above"

# One key per domain and day, so a second try replays the first answer instead of charging twice.
idempotency="register-$DOMAIN-$(date -u +%Y%m%d)"
body="$(jq -nc --argjson cost "$COST" '{cost: $cost, agreeToTerms: "yes"}')"
result="$(pb -X POST -H "Idempotency-Key: $idempotency" -d "$body" "$PORKBUN_API/domain/create/$DOMAIN")"
printf '%s' "$result" | jq 'del(.apikey, .secretapikey)'
pb_ok "$result" || die "the registration failed"

say "$ME: registered. The account's record:"
pb_domain | jq '{domain, status, createDate, expireDate, autoRenew, whoisPrivacy, apiAccess, securityLock}'
