# The Builder's gate test

Run 2026-11-16 19:40 (Brisbane) by `scripts/cutover/builder-gate --lab`, through https://lab.omarchy-mac.org: the builds asked of labd, built by lab-builder on the mini, the mini watched over SSH by `scripts/mini/build-watch`.

- the kernel is built from https://github.com/omacom/linux-aurora with m2-max's running config; the packages from the recipe gate-recipe
- At 10 cores a build:
- the cold kernel build (build b1116-194131-5261): done in 664 s by this Mac's clock; its VM was up 629 s with 10 cores and 16G; the VM's process held 29.7 GB at most, and the whole mini had 33.1 GB in use at most
- labd's pages during the cold kernel build: 1 ms at the 95th percentile at the slowest of 57 readings, the slowest request 6 ms; with 1073741824 bytes going up at 135.7 Mbit/s, 1 ms
- the warm rebuild (build b1116-195205-4423): done in 52 s by this Mac's clock; its VM was up 51 s with 10 cores and 16G; the VM's process held 20.4 GB at most, and the whole mini had 29.8 GB in use at most
- labd's pages during the warm rebuild: 1 ms at the 95th percentile at the slowest of 5 readings (told, not judged)
- the package build (build b1116-195257-fcfc): done in 434 s by this Mac's clock; its VM was up 433 s with 10 cores and 16G; the VM's process held 30.3 GB at most, and the whole mini had 33.3 GB in use at most
- labd's pages during the package build: 1 ms at the 95th percentile at the slowest of 39 readings, the slowest request 6 ms; with 1073741824 bytes going up at 137.0 Mbit/s, 1 ms
- the volume maclab-cache holds 12.3 GB of its 40.0 GB
- the volume maclab-pkgcache holds 13.5 GB of its 120.0 GB
- the artifact store holds 16.1 GB; its budget is 20.0 GB (told, not judged: labd clears it by itself)
- 10 cores a build: labd's pages took 1 ms at the 95th percentile while the Builder's gate test built, under 100
- deploy/builder-cpus already says so
- the mini's set-up gives the Builder its cores: run scripts/mini/setup there after the Deploy

Result: pass
