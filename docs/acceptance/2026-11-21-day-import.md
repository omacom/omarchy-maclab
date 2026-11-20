# The day's import (step 22)

Run 2026-11-21 02:09 (Brisbane) by `scripts/cutover/rehearsal-import run --dumped` on the mini, labd v0.2.0-rc.20, from the final dump taken after the Freeze. Counts only.

- Imported the dump (taken 2026-11-20T16:08:11Z): 148 reports (0 hidden, 22 Verified runs), 37 Mac sign-ins, 148 digests; 55 people, 0 of them new to the lab.
- Every report passes the lab's rules.
- reports: 148 at the old site, 148 in the dump, 148 in reports.db
- hidden reports: 0 in the dump, 0 in reports.db
- tester_bindings: 37 at the old site, 37 in the dump, 37 Mac sign-ins in reports.db
- testers: 32 at the old site, 32 in the dump; the 29 accounts they and the sign-ins name: 29 are people. 27 handles never signed in there: each is a person if GitHub knows it
- The row counts match.
- That Mac's machine id, recomputed from its public key, is on 3 reports and 0 Mac sign-ins.
- The old site's link deleted its report on the lab, and the report's digest is kept.
- reports listed with a reason and carried all the same: 0

Result: pass
