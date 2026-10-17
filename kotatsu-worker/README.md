# kotatsu-worker

A small Kotlin/Javalin sidecar that exposes the
[kotatsu-parsers-redo](https://github.com/Kotatsu-Redo/kotatsu-parsers-redo)
library over HTTP, so Kaodoku can use its sources alongside its own scrapers.

## What it does

The worker wraps the parser library and serves a shmol JSON API that
Kaodoku's Go client then calls. Can also use the same Flaresolverr and
Browser worker like the main server does.

## Setup

The worker runs as a container in the Compose stack. The wiring is in the
repo's [`docker-compose.example.yml`](../docker-compose.example.yml); the
relevant piece:

```yaml
kotatsu-worker:
  build: ./kotatsu-worker
  environment:
    WORKER_KEY: change-me            # required; must match Kaodoku's API key
    FLARESOLVERR_URL: http://flaresolverr:8191   # optional
    BROWSER_WORKER_URL: http://browser-worker:8192  # optional
```

Then point Kaodoku at it:

```yaml
KAODOKU_KOTATSU_WORKER_ENABLED: "true"
KAODOKU_KOTATSU_WORKER_ENDPOINT: http://kotatsu-worker:8484
KAODOKU_KOTATSU_WORKER_API_KEY: change-me   # must equal WORKER_KEY
```

Once both are up, open **Sources** in Kaodoku and import the kotatsu sources you
want.

### Environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `WORKER_KEY` | — (required) | Shared key; callers must send it as `X-Worker-Key`. |
| `PORT` | `8484` | Listen port. |
| `FLARESOLVERR_URL` | unset | FlareSolverr endpoint; enables Cloudflare clearing. |
| `FLARESOLVERR_TIMEOUT_MS` | `60000` | Per-request FlareSolverr timeout. |
| `BROWSER_WORKER_URL` | unset | Selenium browser-worker endpoint for JS parsers. |
| `BROWSER_WORKER_KEY` | unset | Sent to the browser-worker when it requires a key. |

## Build locally

```bash
./gradlew --no-daemon build        # compile + test
./gradlew --no-daemon installDist  # produces build/install/kotatsu-worker
```

## Thanks

This sidecar is only a thin adapter — all the real work of supporting a BUNCH of
sources lives in [kotatsu-parsers-redo](https://github.com/Kotatsu-Redo/kotatsu-parsers-redo).
Huge thanks to its developers for building and maintaining it!!!
