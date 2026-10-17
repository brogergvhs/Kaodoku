<div align="center">
  <img src="assets/icon-rounded.png" alt="Kaodoku" width="150" />
  <h1>Kaodoku</h1>
</div>

A self-hosted manga server. Point it at the sites you read, add the titles you
want, and Kaodoku keeps them downloaded — fetching new chapters as they release
and storing everything as CBZ on your own disk. It ships a web UI to browse,
read, and track your progress, optionally syncing your AniList account.

It is *your* library on *your* server: nothing is streamed from or to third parties
at read time, and your files stay portable CBZ you can take anywhere.

## What it does

- **Automatic downloads.** Add a title and Kaodoku discovers its chapters and
  downloads the missing ones. A scheduler keeps checking for and pulling new
  releases, so your library stays current without manual work.
- **Read in the browser.** A built-in long-strip reader integrated into the UI.
  Both chapters and volumes are supported.
- **Read on iPhone & iPad.** The companion [App](./kaodoku_ios) connects to your
  server to read downloaded manga on iOS — online or fully offline.
- **Track your progress.** Automatic reading state per user through the reader.
  If you are a veteran who has been using AniList for a while and don't want to
  lose anything - connect the personal account to sync progress both ways.
- **Organize.** Saved library "screens" (filtered views), and Collections that
  group titles by shared author, by AniList relations (sequels/side-stories), or
  into your own custom lists.
- **Multi-user.** An admin manages users and roles; each user gets their own
  reading progress, favourites, AniList link, and content guards (block tags,
  genres or adult titles).
- **Import what you already have.** Point it at existing folders of `.cbz` files
  and track them as titles.
- **Handles hard sites.** Optional Cloudflare solving (FlareSolverr) and a
  headless-browser worker for JavaScript-rendered readers.

## How it works

Kaodoku is a single Go binary that serves the web UI and runs a background job
scheduler. State lives in one SQLite database. Downloaded chapters are written
as CBZ files under a downloads directory you mount.

The download pipeline, per tracked title:

1. **Discover** — the linked source is scraped for the chapter list.
2. **Download** — missing chapters are fetched and packed into CBZ.
3. **Scan** — files on disk are indexed so the library reflects reality.

Sources are scraper backends for the sites you read, built on five scraper
engines: `mangadex`, `madara` and `iken` (site templates), `comickz`, and a
`generic` scraper that handles many other sites. Title metadata, search, and
relations come from AniList (available without linking an account, through their
public API).

### Built-in sources

Kaodoku ships 15 ready-to-use sources; you can also add your own on top of that
in the web UI or configs.

### Helper services

Two optional services make difficult sites work:

- **FlareSolverr** — clears Cloudflare challenges.
- **browser-worker** (+ Selenium/Chromium) — renders JS-heavy reader pages so
  their images can be extracted.

They only matter for sources that declare a need for them, so you can run a
lighter stack and still use most built-ins:

- **Without FlareSolverr** (browser-worker only): 14 of 15 built-ins work.
- **Without the browser-worker** (FlareSolverr only): 12 of 15 work.

Sources you add yourself may need one or both of these depending on the site.

### Kotatsu sources (optional sidecar)

The optional [`kotatsu-worker`](kotatsu-worker/README.md) sidecar unlocks a
bunch of sources from [kotatsu-parsers-redo](https://github.com/Kotatsu-Redo/kotatsu-parsers-redo).
Enable it, point Kaodoku at the worker, then browse and import those sources
on the **Sources** page. It (can) reuse the same FlareSolverr and
browser-worker helpers, so Cloudflare- and JS-gated kotatsu sources work (mostly).
See the sidecar's README for [setup](./kotatsu-worker).

## Quick start (Docker Compose)

The recommended way to run Kaodoku is Docker Compose. A working example with all
services wired up is in [`docker-compose.example.yml`](docker-compose.example.yml).

```bash
cp docker-compose.example.yml docker-compose.yml
docker compose up -d --build
```

Open <http://127.0.0.1:8080>.

By default the example binds only to `127.0.0.1` (local machine). 

### Single-user vs. accounts

- **No login (default).** If `KAODOKU_ADMIN_PASSWORD` is unset, Kaodoku runs
  single-user with no sign-in.
- **Accounts.** Set `KAODOKU_ADMIN_USER` and `KAODOKU_ADMIN_PASSWORD` to enable
  sign-in. That environment admin is immutable in-app and manages further users,
  roles, and permissions from the **Users** page. Each user then has their own
  progress, favourites, AniList connection, and content guards.

## Using it

Once the server is up:

1. **Add a source.** Go to **Sources** and configure the site(s) you download
   from. A source tells Kaodoku which scraper to use and where to fetch from.
2. **(Optional) Connect AniList.** In **Settings**, register an AniList API
   application (client id/secret) to enable personal-list sync. This is done
   once and then each user then connects their own AniList account from Settings.
3. **Add titles.** Use **Search** to find titles and add it to the
   library, then link it to one of your sources so chapters can be discovered.
   Already have files? Use **Import** to track existing `.cbz` folders.
4. **Let it download.** The scheduler discovers and downloads chapters in the
   background; you can also trigger "Download missing", "Refresh chapters", or
   "Scan files" per title from its actions menu. Watch progress under
   **Management → jobs**.
5. **Read and track.** Open a title and read. Progress is saved
   automatically; mark chapters read/unread, export a chapter or a range as
   CBZ/ZIP, and organize titles into Collections. If AniList is connected, your
   progress syncs.

### Content guards & AniList (per user)

Users can be restricted to hide adult titles and block specific tags/genres —
guarded titles are hidden throughout the app. AniList sync respects this: only
titles a user is allowed to view are synced, and a user's list is only
auto-populated with titles they added themselves or have actually started
reading. Titles others added can still be planned manually from the title's
"Sync AniList" action.

## Configuration

Configuration is via environment variables (see the compose example) and a few
`serve` flags. All are optional; sensible defaults apply.

### Environment variables

| Variable | Purpose |
| --- | --- |
| `KAODOKU_ADMIN_USER` / `KAODOKU_ADMIN_PASSWORD` | Enable sign-in and define the environment admin. Unset = single-user, no login. |
| `KAODOKU_DOWNLOAD_DIR` | Where CBZ files are written (mount a volume here). |
| `KAODOKU_DB` | SQLite database path (the `--db` flag takes precedence). |
| `KAODOKU_ENCRYPTION_KEY` | 64 hex chars; encrypts stored secrets (AniList tokens). If unset, a `kaodoku.key` is generated next to the database. |
| `KAODOKU_OIDC_ISSUER` / `_CLIENT_ID` / `_CLIENT_SECRET` / `_REDIRECT_URL` | OIDC single sign-on (requires the admin password as break-glass). See the compose example for the optional role-mapping and auto-provisioning knobs. |
| `KAODOKU_BROWSER_SOLVER_ENABLED` / `_ENDPOINT` / `_TIMEOUT_SECONDS` | FlareSolverr for Cloudflare-protected sites. |
| `KAODOKU_BROWSER_DOWNLOADER_ENABLED` / `_ENDPOINT` / `_TIMEOUT_SECONDS` | Headless-browser worker for JS-rendered readers. |
| `KAODOKU_KOTATSU_WORKER_ENABLED` / `_ENDPOINT` / `_API_KEY` | Kotatsu sidecar; the API key must match the worker's `WORKER_KEY`. See [`kotatsu-worker/README.md`](kotatsu-worker/README.md). |
| `XDG_CONFIG_HOME` | Config directory (source definitions, profiles). |

### `serve` flags

```
--addr            HTTP listen address (default 127.0.0.1:8080)
--db              SQLite database path
--refresh-every   Discover-new-chapters schedule, e.g. 1h (0 disables)
--download-every  Download-missing schedule, e.g. 10m (0 disables)
--scan-every      File-scan schedule, e.g. 30m (0 disables)
--run-every       Job-runner tick interval, e.g. 5s
```

Schedules can also be set from **Settings** in the UI; the UI values take over
once configured.

## Build from source

Requires Go 1.26+.

```bash
go build -o kaodoku ./main.go
./kaodoku serve --addr :8080 --db ./kaodoku.db
```

Most built-in sources work with just the binary. The few that need FlareSolverr
or the browser-worker (see [Built-in sources](#built-in-sources)) require those
services running and pointed at via the `KAODOKU_BROWSER_*` variables above; the
Compose file wires all of them up.

## CLI one-off downloads

Kaodoku started as a CLI downloader and still supports one-off grabs without the
server:

```bash
kaodoku download --url "https://example.com/series/some-manga" \
  --chapter 5 --output ./out --allow-ext "jpg,webp"
```

Run `kaodoku help` (or `kaodoku <command> --help`) for the full command and flag
reference. For everyday use, the self-hosted server above is the intended path.
