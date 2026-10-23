# GoTimes
Go Based RSS Clientstylized after NYT


## Stack
- Go stdlib `net/http`, `html/template`, [HTMX](https://htmx.org) (vendored in `web/static`)
- RSS/Atom parsing: [gofeed](https://github.com/mmcdole/gofeed)
- SQLite via `modernc.org/sqlite` (pure Go), schema managed by [goose](https://github.com/pressly/goose) with SQL migrations embedded from `internal/db/migrations`

## How it works
- **Users** are identified by the `Cf-Access-Authenticated-User-Email` header that Cloudflare Access adds. Only the tunnel can reach the app, so the header is trusted; never publish the port directly. For local dev set `DEV_EMAIL=you@example.com`.
- **Feeds** are shared across users (one fetch per URL). A scheduler polls due feeds every 30s with a bounded worker pool (8), using ETag / Last-Modified, with exponential backoff on failures.
- Feed fetching refuses private/loopback addresses (SSRF guard). `ALLOW_PRIVATE_FETCH=1` disables that for tests.
- Non-GET requests must carry `HX-Request: true` (CSRF guard).

## Frontend
Server-rendered HTML fragments swapped in by HTMX; the small behaviours htmx doesn't cover live in `web/static/app.js` (add dialog, filtering sections, searching dispatches, narrow-screen pane switch). Pinned sections (per user) appear in the nav strip under the masthead and at the top of the Sections list. Buttons are icons from an inline SVG sprite in `web/templates/index.html`. Styling is one hand-written file, `web/static/app.css`, built on tokens (palette, type scale, 4px spacing scale) in a cascade-layer order; components use only tokens. Fonts (Courier Prime, Pirata One; both OFL) are vendored in `web/static/fonts`. The CSP forbids inline scripts and styles, so keep it that way.
Feed article HTML is sanitized (bluemonday) when stored and rendered in the reader pane.

## Layout
`cmd/gotimes` entrypoint · `internal/db` open + migrate · `internal/store` queries · `internal/feeds` fetcher + scheduler · `internal/server` HTTP · `web` templates + static (embedded)

## Develop
```sh
DEV_EMAIL=you@example.com go run ./cmd/gotimes   # http://localhost:4444, data in ./data
go test ./...
```
Add a schema change as a new `internal/db/migrations/0000N_name.sql`.

## Run (Docker / Colima)
```sh
cp .env.example .env   # add your Cloudflare TUNNEL_TOKEN
make up                # scripts/build.sh: starts colima, builds, runs
```
In the Cloudflare dashboard, point the tunnel's route at `http://gotimes.internal:4444` and put an Access policy on the hostname.
The SQLite database lives in the `data` Docker volume.
