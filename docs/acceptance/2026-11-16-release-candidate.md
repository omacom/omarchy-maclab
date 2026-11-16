# The Release candidate built; step 1 done, 2026-11-16

Ready 7.

- Step 1 (Cutover): the old repository's five open pull requests are carried over as omacom/omarchy-maclab#2 to #6 (open).
- `build` merged into `main` (PR #1, merged 2026-11-16 22:28 Brisbane by fast-forward to 837e1a83, CI green on all 12 jobs); `main` then protected: the 12 checks required, changes by pull request, no force push or deletion, admins included.
- Tag `v0.2.0-rc.7` on `main`; the Release workflow (run 43866925846) built the reset tool and the bundle; its "Sign and publish" job ran once the `release` environment was approved.
- The release: a pre-release with 12 files; "latest" is not set to it. `release/verify.sh <download> release/allowed_signers v0.2.0-rc.7`: "verified v0.2.0-rc.7: 6 files and lab-agent, against release/allowed_signers"; `lab version`: "lab v0.2.0-rc.7".
- On the mini: the release key of `release/allowed_signers` pinned in place of the local candidates' throwaway keys; `scripts/mini/deploy v0.2.0-rc.7` from GitHub: "Deployed v0.2.0-rc.7 on the mini: labd answers on its loopback port, on its socket and through the Front door; the Builder and the Out-of-band controller are as labd expects. v0.2.0-rc.6 is kept as the way back."; the set-up after it: nothing to change.

Result: pass
