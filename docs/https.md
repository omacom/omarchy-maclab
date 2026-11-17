# How the lab is reached

Omarchy Mac Lab answers at one address, **https://lab.omarchy-mac.org**, the Lab address. The public results, the site,
`lab` and its MCP server, the Macs' agents, uploads and downloads all cross it. This guide says what that path is, what it
allows, and what each program does to fit inside it.

## One path

- The Lab address reaches the mini through one Cloudflare tunnel. `cloudflared` on the mini dials out; no port is open at
  home, and the home address is never published.
- There is no second path for Macs or for big files, and nothing needs Tailscale. The lab's own sign-in is the only gate.
- Everyone dials labd: browsers, `lab`, the agents on the Macs, the Builder. Macs make outbound requests only (check-in
  long polls, reports, uploads, downloads with resume); nothing connects *to* a Mac. labd dials the Out-of-band
  controller on the mini itself, which never crosses the tunnel.
- The edge sets two limits that everything below works inside: a response with no body byte for 125 s is cut (headers do
  not count), and a request body over 100 MB is refused.

## What labd listens on

labd serves no TLS and listens on its own machine only.

| flag | what it does |
|---|---|
| `--listen 127.0.0.1:7770` | The loopback port, for callers on the mini: the Builder, break-glass, the mini's own scripts. An address beyond the machine is refused at start |
| `--socket PATH` | The tunnel's Unix socket, which `cloudflared` dials. It is made for the account labd runs as alone, and it is the only place the edge's `CF-Connecting-IP` is believed. Needs `--public-url` |
| `--public-url https://lab.omarchy-mac.org` | The Lab address. Every link labd builds names it |
| `--alias maclab.omarchy.org` | The Alias, the one other name the tunnel carries |
| `--preview` | The lab is in Preview: answering at the Lab address, closed, before it opens. Every answer tells crawlers not to index it |

`GET /healthz` answers 200 `ok` with no credentials.

## Two names, and no other

- Through the socket labd knows exactly two names. The Lab address is served. Any third name gets 421 with a line naming
  the Lab address.
- **The Alias**, `maclab.omarchy.org`: a page request, `/install` included, gets a 308 to the same path and query at the Lab
  address. Anything under `/api/` is refused: `Omarchy Mac Lab is at https://lab.omarchy-mac.org: use that address`. No
  cookie is set, no credential is read, no sign-in starts, and the lab never prints the Alias. `lab login` at the Alias
  fails plainly and stores nothing.
- At the loopback port labd answers to `localhost` or a loopback address. To ask through the socket by hand, name the Lab
  address: `curl --unix-socket PATH http://lab.omarchy-mac.org/healthz`.
- A request under `/api/` sent as early data (`Early-Data: 1`) gets 425, so nothing that changes the lab is replayed.

## Long calls

Nothing stays silent through the tunnel.

- A held response sends a Keepalive at once and then every 30 s. The event stream (`/api/stream`) keeps its own 15 s.
- `lab exec` and `lab installed freeze-entry` are answered as a stream (`application/x-ndjson`): Keepalives, then the
  result as the last line. An error after the first byte is in the body.
- A job wait is one call of at most 50 s, which labd ends early when the job has more to show. `lab wait`, `lab run` and
  the MCP tools ask again until the job is done.
- A followed build log is polled from an offset (`/api/builds/{id}/log?offset=N&wait=50`).
- A cut `lab exec` is never sent again: `lab` says `connection lost; the command may still be running`. A command
  already started on the Mac runs to its own end. Work that must survive a cut goes in a job.

## Big files

- An upload goes in numbered parts of 32 MiB; anything smaller is one request. Each part is tried up to 8 times. A
  SHA-256 closes the upload, and labd stores the file only if it is the one named. Cut, the same command resumes: it
  asks which parts labd holds and sends the rest. An upload left unfinished is deleted after 24 hours.
- labd refuses any single body over 90 MB, and no client sends a body without `Content-Length`.
- How many parts go at once is labd's `--parts-at-once`, 1 or 4, set from a measurement taken outside the house.
  `MACLAB_PARTS_AT_ONCE` and `MACLAB_UPLOAD_PROGRESS` are `lab upload`'s knobs for that measurement, and
  `MACLAB_UPLOAD_HOLD` for the test of a cut upload; none is a setting for daily use.
- Downloads come from the mini through the tunnel, resume with range requests, and are checked against the SHA-256 the
  lab recorded. They are never cached at the edge.

## Who a request is from

- A person is their GitHub sign-in on the site, or a Lab token in `lab`. A Mac is its device secret; the Builder has a
  token of its own; a Join command carries a one-time enrollment token. A forwarded header or a source address is never
  who a request is from.
- labd refuses any request but GET, HEAD and OPTIONS whose `Origin` names another host, and every form carries a token
  of the session, so another site's page cannot act for a signed-in person.
- Every request is logged with its address, the person or Mac, and the edge's ray id. The caller log is kept 30 days. The
  secret in a report's deletion link never appears in it.
- Refused credentials are counted by address: past 10 in 10 minutes, further bad ones get 429 for 15 minutes and the
  owner is told once. A valid credential always passes.

## What a client does

| client | names itself | plain http or a private CA, for development only |
|---|---|---|
| `lab`, `lab mcp` | `User-Agent: lab/<release>`. A `lab` that does not name itself is refused: "This `lab` was not built for Omarchy Mac Lab", with the install line. One older than the lab's minimum release is refused in one line with the install line | `lab login <url> --allow-plain-http`, `--ca-file F` (both saved with the login), or `MACLAB_ALLOW_PLAIN_HTTP=1`, `MACLAB_CA_FILE` |
| `lab-agent` | its own number and the Lab release it came from, at every check-in | `setup … --allow-plain-http`, `--ca-file F` (kept in `agent.json`) |
| `lab-builder` | by its builder token | `--allow-plain-http`, `--ca-file F` |
| `omarchy-m-test` | `User-Agent`, with its release | `--site` names another address |

Every client:

- verifies TLS against the system roots, plus a private CA if you give one. None can turn verification off;
- follows no redirect: a 3xx answer is an error, so a token never goes anywhere it was not sent;
- sends plain `http://` only to a literal loopback address or `localhost`, unless told otherwise as above;
- ignores `http_proxy` and `https_proxy`, and refuses an address with credentials in it;
- reports a challenge or an error page from the edge as one plain line with the status and the ray id, never the page.

`lab` keeps one login, for the address it was saved for. `MACLAB_URL` and `MACLAB_TOKEN` given together reach another
address for one call: a labd under development, a rehearsal host, the owner's break-glass on the mini. `MACLAB_URL`
alone, naming another address, is refused.

The Python tools in `tools/bt2/`, `scripts/boot-test/` and `scripts/workflow/` came with maclab and carry their own copy
of this policy (`tools/bt2/bt2/labhttp.py`; a test fails if a copy drifts). They do not read `lab`'s login, and they
read `lab exec` in its silent form, which only the loopback port gives: they run on the mini.

## Joining a Mac

- The person who lends a Mac makes its **Join command** on "Lend a Mac", signed in; a Maintainer runs `lab enroll <name>`
  for a Lab-owned Mac. Either is one line, good for one Mac, once, for 1 hour.
- The line is `curl -fsSL https://lab.omarchy-mac.org/join | LAB_JOIN=enr_… bash`. It holds a one-time enrollment
  token, `enr_…`, in the environment of the person's own shell, readable by them and root only. The script takes it
  out of the environment before it starts anything; then the token travels only in an `Authorization` header, never
  in an address, and is in no process's arguments: curl reads that header on stdin (`-H @-`), and setup reads the
  token on stdin too. The line starts with a space, so a shell that skips such lines leaves it out of its history.
- `/join` is the Join command's script: no secret, served to anyone, never kept (`no-store`), made for the current
  release. Its body is one function called on its last line, so a download cut short runs nothing. It downloads
  lab-agent and its signature from the lab, checks the sha256 and then the signature against the release key written
  into it (`ssh-keygen -Y verify`), before anything runs as root, and runs
  `sudo lab-agent setup --server https://lab.omarchy-mac.org --token-stdin`; setup and sudo ask on the terminal, so
  they work under `curl | bash`. A lab with no signed agent makes no Join command.
- `lab-agent setup` runs its pre-checks, then shows the terms and needs a typed `yes` on the terminal before it changes
  anything or tells the lab anything about the Mac. Without a terminal it stops, unless `--accept-terms <sha256>` names
  the exact terms read at `/terms`. The lab records the acceptance with the person, the Mac, the time and that sha256.
- The token is used up by the first enrollment and bound to that Mac; another Mac with a copy is refused. A setup that
  stopped half way can run again on the same Mac until its first check-in.
- `lab enroll <name> --revoke`, or Cancel on the page, withdraws an unused Join command.
- `sudo lab-agent update`, at the Mac, takes a newer agent from the lab with the Mac's own device secret. An agent with a
  bad signature, or one older than the agent installed, is refused, and nothing is changed. Nothing pushes an agent to a
  Volunteer's Mac; a Maintainer updates a Lab-owned Mac's while it is idle with `lab agent-update <mac>`, which needs
  lab-agent 0.10.0 or later on the Mac.

## If the Lab address ever changes

`lab-agent move` is only for a change of the Lab address. A Mac joins another lab with that lab's Join command, never
with `move`.

- `sudo lab-agent move --server <the new Lab address>`, in a terminal on the Mac or over SSH, never through `lab exec`:
  run from inside lab-agent's own service it refuses before it changes anything.
- Unless `--no-check` is given, `move` first asks the new address, with the Mac's own device secret, whether it knows
  this Mac as the same kind of Mac. Otherwise it stops with "Nothing was changed".
- It replaces only `server`, `server_ca` and `allow_plain_http` in `/etc/maclab/agent.json`, in one fsynced rename, and
  restarts only `lab-agent.service`. The Mac keeps its enrollment, its GUI settings and its recovery state.
- A Mac has moved when it checks in at the new address (`lab status`).

## While the lab is Dark

When Cloudflare, the tunnel or the home link is gone, the lab is Dark for everyone, Maintainers included. There is no
second way in.

- `lab` says "Omarchy Mac Lab cannot be reached right now." and that no reset is possible until it returns.
- `omarchy-m-test` still runs in full on any Mac. Only the upload waits: "Your report is saved at `<path>`." and "Upload it
  later: omarchy-m-test --upload `<path>`".
- The Macs' agents keep retrying and report when the lab returns. A Lab boot comes back to the Known-good kernel by
  itself, lab or no lab.
- A visitor gets a stale copy of a public page for up to a day, or the edge's own error page.

## What may be cached

labd states a policy on every response; the default is `no-store`.

- A public page or export, for a visitor with no session: cached at the edge, fresh for 60 s, then served stale while the
  edge refreshes, for up to a day. A browser always revalidates.
- Anything rendered for a session: `private, no-store`.
- `/install` and `/install/lab`: public, 5 minutes, the same for whoever asks. In Preview both serve the Release
  candidate the mini runs; after the opening, the latest Lab release. Fingerprinted assets: public, one year.
- `/api/*` is never cached, the exports under `/api/v1/` excepted. Artifacts say `private` themselves.
- A request with a Lab token or the session cookie is never answered from the edge's cache, whatever the path.
- The edge passes labd's `max-age` to the browser as it is (Browser Cache TTL: Respect Existing Headers).

## A labd of your own

```sh
labd --sign-in <folder>                  # loopback only: http://127.0.0.1:7770
labd break-glass --account-id <id> <your-handle>
lab login http://127.0.0.1:7770          # replaces the saved login; or keep it and give MACLAB_URL with MACLAB_TOKEN
lab-agent sim --server http://127.0.0.1:7770 --token <enrollment token>
```

`tools/fake-github` stands in for GitHub at a loopback address (`labd --github-url`), for sign-in without the real one.
The tunnel, the zone and the edge's settings are scripts in `scripts/cloud/` and `scripts/mini/`; a labd of your own
needs none of them.

## Operational limits

### The clock comes before the certificate

- After a reboot a Mac's clock can start in 1976 until NTP corrects it, about 25 s later. Until then every TLS certificate
  looks not yet valid, so the agent's check-ins fail.
- The agent logs that it is a clock problem, retries every 5 s, and gets through once the clock is right.
- A Lab boot has twice its boot timeout to reach labd (12 min by default). Those deadlines are absolute times, so a clock
  in the past doesn't fire them early.
- What this needs is NTP reachable from the Macs. A Mac whose clock never comes right can't reach the lab at all.

### Long requests

- `lab exec` runs up to its `--timeout` (120 s unless given, at most an hour). Its answer is held with Keepalives for as long as it runs.
- Job waits are asked in calls of at most 50 s.
