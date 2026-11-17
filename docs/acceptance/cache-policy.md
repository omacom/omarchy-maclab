# The cache policy and its header pair (spec, section 6 "Cache")

Chosen 2026-11-15 (Brisbane). The spec leaves the exact pair to the build and asks that the edge's lifetime go in the CDN-only header. This is what labd sends; FD 13 proves it at the edge and its output goes below.

## What labd says, on every answer

| Answer | Headers |
|---|---|
| A public page or an export, read (`GET`, `HEAD`) with no session cookie and no `Authorization`, answered 200 and setting no cookie | `Cache-Control: public, max-age=0` and `Cloudflare-CDN-Cache-Control: max-age=60, stale-while-revalidate=86400, stale-if-error=86400` |
| Anything asked for with the session cookie, known to labd or not, and any page rendered for a session | `Cache-Control: private, no-store` |
| A fingerprinted file under `/assets/` | `Cache-Control: public, max-age=31536000, immutable` |
| `/install`, `/install/lab` | `Cache-Control: public, max-age=300` |
| An artifact | `Cache-Control: private` |
| Everything else: `/api/*` but the exports, `/join`, `/status`, a deletion link, sign-in, every error, a forward on the Alias | `Cache-Control: no-store` |

The public pages are `/`, `/matrix`, `/models`, `/features`, `/gaps`, `/aurora`, `/benchmarks`, `/candidates`, `/reports`, `/data`, `/terms`, `/privacy` and what is under `/models/`, `/features/`, `/candidates/` and `/reports/`, never a path ending in `/deletion`. The exports are `/api/v1/reports`, `/api/v1/checks`, `/api/v1/matrix`, `/api/v1/benchmarks` and `/api/v1/aurora`, each also with its `.json` or `.csv` ending. `internal/server/edge.go` holds the list; a test fails when it and the first cache rule of `scripts/cloud/edge.json` name different paths.

## Why this pair

- **The edge's lifetime is in `Cloudflare-CDN-Cache-Control`.** Cloudflare reads it in place of `Cache-Control`, keeps to it, and does not pass it on to the browser. Fresh for 60 s; then the stale copy is served while the edge fetches the page again in the background, for up to a day; and for a day while labd answers 500, 502, 503 or 504.
- **Nothing that ends stale serving is in a header the edge reads.** `s-maxage`, `must-revalidate`, `proxy-revalidate` and `no-cache` each cancel `stale-while-revalidate` or `stale-if-error` at Cloudflare. None of them is sent.
- **The browser asks again every time.** `max-age=0` makes its copy stale at once, and the page carries no `ETag` or `Last-Modified`, so asking again is a full request: a person who signs in never gets the signed-out copy back from their own browser. `no-cache` would say the same, but it is one of the words that end stale serving wherever the edge reads `Cache-Control`, so it stays out.
- **Only a 200 is kept.** An error says `no-store`, so the edge never replaces a good copy with a bad one and `stale-if-error` has a copy to serve. A page that is not there (a deleted report among them) is not kept either.
- **No forced lifetime at the edge.** The cache rule only makes the public paths eligible; the lifetime is labd's own header. A forced lifetime would keep a page whatever labd said of it.

## The guard

Nothing rendered for a session is kept. labd decides by the request, as the edge's rule does: the text `lab_session=` in a `Cookie` header, whether or not labd knows that session. So the two cannot disagree about a request, and a stale cookie costs only a cache miss. An answer that sets a cookie is never given the pair. A request with `Authorization` is never given it.

## The exports and `/api/*`

The spec says exports are cached like the public pages and that one rule bypasses cache on `/api/*`; the exports live under `/api/v1/`. Both now hold: in `scripts/cloud/edge.json` the first cache rule lists the eleven export paths beside the pages, and the bypass covers everything under `/api/` except one of those paths asked for without the session cookie. The skip rule on `/api/*` is unchanged.

## A Lab token, the session cookie, and the browser's copy

FD 13 through the real edge on 2026-11-16 found two things labd's headers alone do not hold. A public page asked for with a Lab token was a `HIT`: the edge keeps a page by the request's path and cookie, and an `Authorization` header is neither. And every public page reached the browser as `public, max-age=14400`: the zone's Browser Cache TTL was Cloudflare's default of four hours, which rewrites a lower `max-age`. So the zone script now sets Browser Cache TTL to Respect Existing Headers (0), the first cache rule also leaves out a request with an `Authorization` header, and a third rule, the last, bypasses cache for any request with an `Authorization` header or the session cookie, on every path. Cloudflare applies every cache rule that matches, a later one over an earlier one, so the third wins wherever it matches.

## What was run, 2026-11-15

`go test ./internal/server -run 'Kept|Cacheable|Policy|HeaderPair|CacheRules'` and `go test ./internal/cloudtest` on the M4 Pro: every public page and export of a lab holding the old site's production reports, by `GET` and `HEAD`, in Preview and out of it, carries the pair; the same paths with a session, with a cookie labd does not know, and with a Lab token do not; no route under `/api/` but an export, no error, no forward on the Alias, `/status`, the deletion link and sign-in ever do.

Not shown here, and owed by FD 13 (slice 51): that Cloudflare on the Free plan honours the pair as its documentation says. To record: `cf-cache-status` for a signed-out public page (`MISS`, then `HIT`, then `UPDATING` after 60 s), for an export, for the same page with the session cookie (never `HIT`), for `/api/auth` (never `HIT`), and for a fingerprinted file (`HIT`). If the page is never served stale, the pair is what to change, in `internal/server/edge.go`; this file and its test follow.
