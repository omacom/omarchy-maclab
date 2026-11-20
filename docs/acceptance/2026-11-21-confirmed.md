# The Confirmed checklist

Run 2026-11-21 07:13 (Brisbane) by `scripts/cutover/confirmed --run`, against https://lab.omarchy-mac.org.

- 1. pass: the smoke script is green against https://lab.omarchy-mac.org: installed, uploaded a golden report, saw it, deleted it by its link
- 2. pass: the old install line gives the lab's installer, which takes the latest Lab release, 0.2.0
- 2. pass: the old report link lands on its page at the lab through omarchy-m-testing.org, path and query kept
- 2. pass: the old deletion link lands on its page at the lab through omarchy-m-testing.org, path and query kept
- 3. pass: the lab's Sign in link names the existing OAuth app
- 5. pass: on the mini: one backup by hand and restored; the daily summary recorded for the Lab group; no Dark minute since the lab opened

The owner's to look at, each seen before the dated line is written:

- 3. Signed in with the existing OAuth app; People shows the 14 marked.
- 4. Each Lab-owned Mac shows as signed in; the lab has published one Verified run from each.
- 5. The daily summary arrived in the Lab group; healthchecks.io shows the Dead-man switch green since the lab opened.

Result: pass, what a script can check
Confirmed: 2026-11-21, Marcelo Alcantara
