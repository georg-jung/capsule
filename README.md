# Capsule

Capsule is a small, self-hosted web app for keeping standalone HTML files available everywhere—including offline. Everything is private behind passkey authentication.

## What it does

- The first successful passkey registration claims a fresh instance and chooses its private app name.
- Existing owners create single-use invite links that expire after exactly six hours.
- Every registered passkey is an equal owner. Owners can invite people, rename passkeys, and remove any owner except the passkey behind their own current session.
- The main screen is both an ergonomic file index and a file manager, with multi-file input, drag and drop, atomic same-name replacement, rename, and delete.
- Uploaded files keep native same-origin browser capabilities, including shared `localStorage` and IndexedDB.
- The installable PWA synchronizes every current file automatically. Offline mode is read-only and reports whether the offline copy is complete.
- All persistent state—SQLite, sessions, passkeys, and content-addressed file objects—lives in one Docker volume.

## Deploy

Capsule expects a reverse proxy to terminate TLS. It does not depend on Traefik, Caddy, nginx, or forwarded headers. Passkeys require the exact external origin to be configured explicitly.

```sh
git clone https://github.com/georg-jung/capsule.git
cd capsule
export CAPSULE_ORIGIN=https://capsule.example.com
docker compose up -d
```

The included Compose file binds to `127.0.0.1:8080` by default. Point the host reverse proxy there, or override `CAPSULE_BIND`/Compose networking to suit the deployment.

```sh
CAPSULE_BIND=0.0.0.0:8080 docker compose up -d
```

For a Docker-network reverse proxy, remove the published port in an override and attach the service to the proxy's external network. The container itself always listens on port 8080.

### Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `CAPSULE_ORIGIN` | required | Exact public origin, such as `https://capsule.example.com`; HTTP is accepted only for loopback development. |
| `CAPSULE_LISTEN_ADDR` | `:8080` | Internal HTTP listen address. |
| `CAPSULE_DATA_DIR` | `/data` | Persistent data directory. |
| `CAPSULE_MAX_UPLOAD_MB` | `100` | Maximum size of each individual uploaded file. |
| `CAPSULE_BIND` | `127.0.0.1:8080` | Host bind used by `compose.yaml`. |

Two instances can run side by side using different origins, bind ports, Compose project names, and volumes.

### Updating

The `latest` image is published from `main` to `ghcr.io/georg-jung/capsule:latest`, alongside immutable `sha-*` tags and semver tags. Watchtower or another container replacement tool can follow `latest`; the SQLite schema and data volume persist across replacement.

Back up the named Docker volume as a unit. SQLite uses WAL mode, so stop the container or use a volume-aware snapshot to obtain a consistent filesystem backup.

### Lost every passkey

An operator with Docker access can reset authentication while preserving every uploaded file. Stop the serving container first so no passkey ceremony or upload can race with recovery:

```sh
docker compose stop capsule
docker compose run --rm capsule reset-auth
docker compose start capsule
```

The instance becomes unclaimed. Claim it immediately from a trusted browser before exposing it again.

## Security model

Capsule has three deliberate trust boundaries:

1. A fresh public instance has no way to identify its intended owner. The first **successful passkey registration** wins the atomic claim. Deploy and claim it immediately.
2. Uploaded HTML is trusted owner code. Native shared `localStorage` means it shares Capsule's origin and can act with the current owner's authority. Upload only files you trust.
3. Offline files remain readable to the browser profile that synchronized them, even if the server later expires or revokes that session while the device is offline. Explicit logout clears Capsule's private caches; an unreachable offline device cannot receive server-side revocation.

Sessions use random bearer tokens stored only as hashes, `HttpOnly`/`SameSite=Strict` cookies (`Secure` in production), exact-origin checks, and CSRF tokens. Invite secrets are placed after the URL fragment so they do not enter proxy logs or referrer headers; the browser exchanges and removes the fragment before registration.

## Development

The runtime is Go with standard-library HTTP/templates, pure-Go SQLite, and `go-webauthn`. The browser interface is dependency-free JavaScript and CSS. Playwright is development-only.

```sh
# Unit and HTTP integration tests, including the race detector
docker build --target test .

# Full Chromium lifecycle with virtual passkeys and offline mode
npm ci
npx playwright install chromium
npm test
```

The Playwright test builds and starts an isolated production container automatically. CI repeats Go, browser, and container smoke checks on every pull request.

## License

[MIT](LICENSE)
