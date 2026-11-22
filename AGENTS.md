# omarchy-maclab

Omarchy Mac Lab tests Omarchy on real Apple Silicon Macs and publishes what it finds. One repository holds it all: the lab's Go programs (`cmd/`, `internal/`), the `omarchy-m-test` command (`cli/`), the report schema (`schema/`), the feature catalogue (`catalogue/`), the installer and the release build. `site/` is the Rails site the Go port is checked against; it goes when the port passes. The spec is `docs/spec/spec.md`.

## Rules

- Commit as the owner: `git -c user.name="Marcelo Alcantara" -c user.email=maralc@gmail.com commit ...`.
- No AI attribution anywhere: no Co-Authored-By trailers, no "Generated with", no model names in commits, PRs, issues, code or docs.
- Privacy is a hard requirement: reports never carry serial numbers, MAC addresses, hostnames, usernames, Wi-Fi network names, IP addresses, disk identifiers, key slots or home-directory content.
- The `omarchy-m-test` command never reboots a machine and never touches disk encryption or boot files; every change it makes is restored.
- The product is Omarchy Mac Lab. `omarchy-maclab` is only this repository, and "maclab" alone means Chris Kearney's project. Use `GLOSSARY.md`'s words and invent none.
- Names an enrolled Mac or a stored record carries keep their `maclab` spelling: the `MACLAB_*` variables, `/var/lib/maclab`, `/etc/maclab`, `~/.config/maclab`, `~/.local/share/maclab`, the systemd units, `com.maclab.oobd`, the kernel command-line keys, the schema ids, the ESP names. `internal/repotest` fails when one changes.
- No Docker, Kubernetes or Node build chain. `builder/Containerfile` and `builder/pkg/Containerfile` are the Builder's VMs and stay.
- Run `scripts/ci-local` before a pull request. It runs what `.github/workflows/tests.yml` runs.
- `scripts/mini/` runs on the mini with only what macOS has: no `python3`, `git` or `cc`, and `brew` only in the set-up and the Maintenance run, never as root; the look at the tools' copy, by the set-up and by `scripts/mini/check`, also runs `otool`, of the Command Line Tools Homebrew needs. No LaunchDaemon file in `deploy/` names a path under `/opt/homebrew`: a Homebrew tool runs from its root-owned copy, `/usr/local/lab/tools/current/<formula>/`. `internal/minitest` runs the scripts against a made-up machine. `scripts/mini/guest/` is the one exception: it runs on the backup guest (Debian), never on the mini.
- `scripts/cloud/` runs on the owner's Mac against Cloudflare and Porkbun. With no option a script prints the API calls and sends nothing; `--check` only reads; `--apply` writes. The Cloudflare token comes from `--token-file` or a hidden prompt: never an argument, the environment or the Keychain, and never printed. The edge settings are `scripts/cloud/edge.json`, and the zone script fails when one drifts. `internal/cloudtest` runs the scripts against a stand-in for Cloudflare's API; no test calls Cloudflare.
- `scripts/cutover/front-door/` holds the Front door's gate tests. With no option a test says its steps and sends nothing; `--local` runs it against a labd it starts on loopback; `--lab` runs it through the Lab address under the saved login and writes its dated record to `docs/acceptance/`. A test never holds a token and never prints an error page. `settle` is the one place the measurements become settings in `deploy/`. `run` runs the thirteen in order and lists what each said. The stream and edge tests (1, 2, 6 to 13) read the wire with `tools/front-door-probe`; under `--local` it also stands in for the edge and the tunnel in front of labd's socket, and `FRONT_DOOR_FAULT` makes it misbehave so that a test can be seen to fail. What they ask the mini is `scripts/mini/edge-facts`, which prints counts and never an address. `internal/cutovertest` runs them all against a labd on loopback; no test reaches the Lab address.
- `scripts/cutover/alias-rehearsal` and `scripts/cutover/dark-recordings` run the same three ways and write `docs/acceptance/DATE-alias-rehearsal.md` and `DATE-dark-recordings.md`. The rehearsal makes and deletes its own throwaway custom hostnames and records through `scripts/cloud/alias.sh` and `rehearsal-record.sh`; the recordings are not gates and write down what the edge did, as seen. A cookie is printed by name only. `scripts/cutover/alias-ask.md` is a draft the owner sends himself.
- `scripts/cutover/out-of-band/` holds the Out-of-band wiring's eight gate tests, in the same three ways (dry run, `--local`, `--lab --mini USER@HOST`). `--local` needs `bin/fake-mini`, the made-up mini of `tools/fake-mini`. `--lab` resets real Macs and stops for the owner's hand: never start it from a session.
- `scripts/cutover/builder-gate` is the Builder's gate test, run from the owner's Mac: it asks labd for the three builds and has `scripts/mini/build-watch` watch the mini over SSH. The cores a build gets are `deploy/builder-cpus`, written only by `settle cores`. `internal/cutovertest` runs it against builds nobody compiles, with made-up times; no test starts `container`.
- `scripts/cutover/day`, `back` and `confirmed` run on the owner's Mac and never do the owner's part: no message, no password, no nameserver, no tag, no deletion. With no option each says what it would check and asks nothing; `--run` stops with status 3 at a step that is the owner's. `open` and `confirmed-mini` run on the mini with sudo. `internal/carrytest` runs the whole day against a labd on this machine with Cloudflare, the old site, GitHub's releases and ssh played; no test reaches any of them.
- `scripts/cutover/hardening/` holds Hardening's fourteen gate tests (Ready item 8), in the same three ways. `--local` needs `bin/fake-github`, `bin/lab-agent` and `bin/hardening-probe`; it signs people in through the stand-in for GitHub and plays a Volunteer's Mac with `lab-agent sim`. A test whose rule another slice has not landed ends as `waits` (exit 4) and names the slice; remove its `WAITS_FOR` line when that slice lands. Test 14 runs once, in step 14a; `run --lab` takes that result. What test 6 asks the mini is `scripts/mini/secrets-seen`, which prints counts and file names, never a secret.
- A Lab release is made by `.github/workflows/release.yml` from a tag the owner pushes. `release/local.sh vX.Y.Z-rc.N` makes one on this machine, signed with a throwaway key, under `dist/release/`; `release/check.sh` checks every file of one. The benchmark programs are built by `release/bench.sh` from the sources `release/bench-pins` pins and `tools/bench-macos/`, on the release workflow's macOS and arm64 Linux runners; `local.sh` builds those this machine can and leaves out the rest, naming why.

## Agent skills

### Issue tracker

Issues live in this repository's GitHub Issues (`gh` CLI), the only tracker. See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` at the repo root and `docs/adr/`. See `docs/agents/domain.md`.
