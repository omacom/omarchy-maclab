# The Release candidates rc.8 and rc.9 built and Deployed, 2026-11-17

Ready 7.

- Carried pull requests: omacom/omarchy-maclab#3 to #6 brought up to `main` (GitHub's update branch) and rebase-merged with CI green on all 12 jobs (Go flakes on #5 passed on a re-run); #7 merged 15:50 Brisbane, #6 to #3 at 17:06. #2 stays open: its `rails-aggregation.json.gz` has to be re-recorded from Rails on `main`.
- Tag `v0.2.0-rc.8` on `main` (43dc6829, CI green); the Release workflow (run 44039829543) built the reset tool and the bundle; "Sign and publish" ran once the owner approved the `release` environment. A pre-release with 12 files, published 17:40; "latest" is not set to it. `release/verify.sh <download> release/allowed_signers v0.2.0-rc.8`: "verified v0.2.0-rc.8: 6 files and lab-agent, against release/allowed_signers"; `lab version`: "lab v0.2.0-rc.8".
- On the mini: `scripts/mini/deploy --dry-run v0.2.0-rc.8`, then `scripts/mini/deploy v0.2.0-rc.8` from GitHub: "Deployed v0.2.0-rc.8 on the mini: labd answers on its loopback port, on its socket and through the Front door; the Builder and the Out-of-band controller are as labd expects. v0.2.0-rc.7 is kept as the way back." `/healthz` through the Lab address: ok; `/`, `/reports`, `/status`, `/install`: 200.
- omacom/omarchy-maclab#8 (a wrong picture from software decode fails even when the screen locks at the second look) merged 19:57 with CI green (a Go flake passed on a re-run).
- Tag `v0.2.0-rc.9` on `main` (a8151572, CI green); the Release workflow (run 44065083477); the `release` environment approved on the owner's word. A pre-release with 12 files, published 20:33; "latest" is not set to it. `release/verify.sh`: "verified v0.2.0-rc.9: 6 files and lab-agent, against release/allowed_signers".
- On the mini: `scripts/mini/deploy v0.2.0-rc.9` drained 3 running jobs, then: "Deployed v0.2.0-rc.9 on the mini: labd answers on its loopback port, on its socket and through the Front door; the Builder and the Out-of-band controller are as labd expects. v0.2.0-rc.8 is kept as the way back." Deployed 20:59. `/healthz` through the Lab address: ok; `/`, `/reports`, `/status`, `/install`: 200.

Result: pass
