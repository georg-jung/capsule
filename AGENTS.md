# Capsule agent guide

Capsule is a small, self-hosted Go application for passkey-gated static HTML files.

## Invariants

- Keep every uploaded file and all application state inside the configured data directory.
- Treat uploaded HTML as trusted, same-origin owner code. Do not silently add a sandbox.
- Every HTTP mutation requires an authenticated session, exact-origin validation, and CSRF validation.
- The first successful passkey registration atomically claims an unclaimed instance.
- Invite links expire after six hours and are consumed by one successful registration.
- An owner may delete other owners but never the owner behind the current session.
- Offline content is read-only. Logout clears Capsule's offline caches.

## Feedback loop

- Go tests: `docker build --target test .`
- Browser tests: `npm ci && npx playwright install chromium && npm test`
- Full local stack: `docker compose up --build`

Prefer tests through the HTTP or browser interfaces. Keep the runtime dependency set small.

## Workflow preference

Simple, low-risk fixes may land directly on `main`; a pull request is not required unless the change benefits from review or coordination.
