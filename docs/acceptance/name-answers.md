# The name answers (Cutover steps 8 and 9)

Two runs belong here. The first is done: the scripts against a stand-in for Cloudflare's API. The second is live and is the owner's: it needs a Cloudflare token, the mini's password and the tunnel, and nothing public was made while the scripts were written.

## Against a stand-in, 2026-11-14 (Brisbane)

`go test ./internal/cloudtest ./internal/minitest` on the M4 Pro, macOS's own bash 3.2. No call reached Cloudflare or Porkbun; the answers are in the shape of Cloudflare's published API schema, not recorded from the zone.

What it shows:

- `scripts/cloud/zone-settings.sh --check` exits 0 on a zone as the spec has it and writes nothing. Each of 45 ways to differ makes it exit 1 and name that one thing only: each of the nine "off" and seven "on" settings (Smart Tiered Cache is two calls) and Browser Cache TTL at four hours; the skip rule missing, with a second rule beside it, without the Browser Integrity Check, or disabled; the rate rule missing, at 3000 requests, over 60 s, limited to one name, or with a second rule; the cache rules missing, without the `/api/*` bypass, without the bypass for a Lab token or the session cookie, without the Lab-token guard, with a forced edge lifetime, as "cache everything", without the session-cookie guard, or with a third rule; a redirect rule; the mail rule missing, off, dropping, forwarding elsewhere, to an unconfirmed mailbox, or with mail routing off; DNSSEC not active; either tunnel alert missing or off.
- `--apply` puts each of those right with one write and ends green, except the owner's three (DNSSEC, the two alerts), which it names and never sets, and a mailbox that has not confirmed. On a zone with nothing right it sets everything, then a second run writes nothing.
- The settings in `scripts/cloud/edge.json` are the spec's two lists, word for word; the rate rule's numbers are read from the spec's sentence.
- `scripts/cloud/tunnel.sh --apply` makes one tunnel whose routes the dashboard does not hold, writes its credential once (0600, or over ssh), and the proxied record of the Lab address; it refuses to write over a file, and makes nothing when the credential has nowhere to go.
- The token: taken from a file only its owner reads or a hidden prompt, on every call, never printed (also under `bash -x`), never an argument. A web error page gives its status and ray id only.
- Each script's dry run is what `internal/cloudtest/testdata/*.dry-run.txt` holds, and sends nothing.
- On a made-up mini: the set-up installs `cloudflared` from Homebrew as the prefix's owner, copies it for root, puts the credential at `/var/lab/secrets/cloudflared.token` (lab only, 0400), removes the copy it was given and starts the tunnel under `deploy/org.omarchy-mac.lab.cloudflared.plist`; Deploy asks the Front door once the tunnel is installed, puts the previous build back when it does not answer, and restarts the tunnel only when the routes changed.

What it does not show: that Cloudflare's API takes each call as written, which rows the token needs, `cloudflared` itself (it was never installed or run), and anything through the real name.

## Live, by the owner

Owed. The commands are in the overnight to-do under "06"; their output goes here:

- `scripts/cloud/zone-settings.sh --check` and `scripts/cloud/tunnel.sh --check`, both green.
- `curl -sI https://lab.omarchy-mac.org/` with Preview's `X-Robots-Tag: noindex`.
- A third name on the tunnel answering 404.
- `dig +dnssec omarchy-mac.org` showing the zone signed.
