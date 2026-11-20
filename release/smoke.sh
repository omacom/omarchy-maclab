#!/usr/bin/env bash
# End-to-end smoke test against Omarchy Mac Lab (or SITE=...):
#   1. install the latest release with the lab's one-line installer
#   2. run it with --dry-run (on a machine that isn't an Apple Silicon Mac it
#      must refuse, run nothing and exit 2; on a Mac it writes the report)
#   3. upload a saved report with --upload, fetch its page, see the same
#      report refused as already uploaded, then delete it via its deletion link
# Needs curl, python3, ssh-keygen. Leaves no report on the lab, only the digest
# it keeps of a deleted one.
set -euo pipefail

site="${SITE:-https://lab.omarchy-mac.org}"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
t="$(mktemp -d)"
report_url=""
deletion_url=""
# confirm FILE: send the deletion page's form as a browser does. The page
# sends no referrer, so the form's Origin is null and Sec-Fetch-Site says
# whose page it is.
confirm() {
  curl -fsS -o "$1" -X POST "$report_url" -H "Origin: null" -H "Sec-Fetch-Site: same-origin" \
    --data-urlencode "_method=delete" --data-urlencode "token=${deletion_url##*token=}"
}
# However this run ends, the report it uploaded does not stay on the lab.
leave() {
  local status=$?
  if [[ -n $deletion_url ]] && ! confirm /dev/null 2>/dev/null; then
    echo "The report this run uploaded is still on $site. Delete it by opening: $deletion_url" >&2
  fi
  rm -rf "$t"
  exit "$status"
}
trap leave EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

echo "== install from $site/install"
curl -fsSL "$site/install" | HOME="$t/home" XDG_DATA_HOME= bash
cli="$t/home/.local/bin/omarchy-m-test"
"$cli" --version

echo "== omarchy-m-test --dry-run"
set +e
(cd "$t" && printf '\n' | HOME="$t/home" "$cli" --dry-run) > "$t/run.log" 2>&1
status=$?
set -e
cat "$t/run.log"
if grep -q "only runs on Apple Silicon Macs" "$t/run.log"; then
  [[ $status == 2 ]] || fail "refusal exited $status, not 2"
else
  [[ $status == 0 && -f "$t/omarchy-m-test-report.json" ]] || fail "dry run exited $status without a report"
fi
grep -q "^Uploaded\. Your report" "$t/run.log" && fail "--dry-run uploaded something"

echo "== upload a saved report with --upload"
# The golden report, signed by this run's own new key: the lab keeps a digest of
# every report it held, so the same one would be refused the second time.
golden="${GOLDEN:-$repo/schema/golden/signed/m2-max-image2.json}"
HOME="$t/home" XDG_STATE_HOME= python3 - "$t/home/.local/share/omarchy-m-test" "$golden" > "$t/saved.json" <<'PY'
import json, sys
sys.path.insert(0, sys.argv[1])
from omarchy_m_test import report, signing
from omarchy_m_test.host import RealHost
with open(sys.argv[2], encoding="utf-8") as f:
    unsigned = {name: value for name, value in json.load(f).items() if name != "signature"}
signed, unsigned_because = signing.sign(RealHost(), unsigned)
if unsigned_because:
    sys.exit(unsigned_because)
sys.stdout.write(report.to_text(signed))
PY
HOME="$t/home" "$cli" --upload "$t/saved.json" --site "$site" > "$t/upload.log" 2>&1 || fail "upload: $(cat "$t/upload.log")"
report_url="$(sed -n 's/^Uploaded\. Your report: //p' "$t/upload.log")"
deletion_url="$(sed -n 's/^Keep this link to delete it later: //p' "$t/upload.log")"
[[ -n $report_url && -n $deletion_url ]] || { deletion_url=""; fail "upload: $(cat "$t/upload.log")"; }
echo "report: $report_url"
curl -fsS -o /dev/null "$report_url" || fail "report page not reachable"

echo "== the same report again is already uploaded"
again="$(HOME="$t/home" "$cli" --upload "$t/saved.json" --site "$site" 2>&1)" || fail "a repeat failed: $again"
[[ $again == "Already uploaded. Your report: $report_url" ]] || fail "a repeat answered: $again"

echo "== delete it via its deletion link"
curl -fsS -o "$t/deletion.html" "$deletion_url"
curl -fsS -o /dev/null "$report_url" || fail "opening the deletion link deleted the report"
confirm "$t/deleted.html"
grep -q "Report deleted" "$t/deleted.html" || fail "deletion didn't confirm"
deletion_url=""
# The edge keeps a public page fresh for 60 s and purges nothing on a delete, so ask past it.
code="$(curl -sS -o /dev/null -w '%{http_code}' "$report_url?after-delete=$RANDOM$RANDOM")"
[[ $code == 404 ]] || fail "deleted report still answers $code"

echo "smoke ok: installed $("$cli" --version), dry run as expected, a saved report uploaded, refused the second time and deleted"
