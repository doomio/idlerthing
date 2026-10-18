# Screenshots

Regenerates the images used in the top-level `README.md`, against a throwaway
database filled with **invented** data.

```sh
npm install playwright
npx playwright install chromium
NODE_PATH=./node_modules go run ./scripts/screenshots
```

Images land in `docs/screenshots/` (override with `OUT=`). It is a dev tool: it
is not part of the binary, and `go build ./...` only compiles it.

## What it does

1. Builds the app into a temp directory.
2. Imports `fixture.go` through the real `idlerthing import`, so the seed is the
   app's own export format rather than hand-poked tables.
3. Serves a mock PulsedMedia customer-API feed over loopback
   (`IDLER_PMAPI_URL`), so no request leaves the machine and the real provider
   is never contacted.
4. Starts the app on a free loopback port with a throwaway database.
5. Drives a headless browser through `shoot.js`: log in, set the synthetic
   provider key, sync, screenshot each page.

## Rules for this directory

- **All data is invented.** `fixture.go` is committed to a public repository
  and uses only the reserved `example.net` / `example.com` domains (RFC 2606).
  Never put a real hostname, price, username, email or API key in it.
- **Never point it at a real database.** `IDLER_DB` is always a temp path.
- `shoot.js` asserts the provider page never echoes the API key back, so a
  regression that exposed it would fail the run rather than bake the secret
  into a committed image.

## Requirements

- Go, to build the app.
- Node with the `playwright` package and its chromium build. `NODE_PATH` must
  point at wherever `npm install playwright` put `node_modules`.
- Chromium's shared-library dependencies. On a minimal container,
  `npx playwright install --with-deps chromium` needs root; without it the
  browser will fail to launch with a missing-library error such as
  `libnspr4.so: cannot open shared object file`.

## Debugging

- `DUMP_FIXTURE=path.json go run ./scripts/screenshots` writes the seed
  document and exits, so the fixture can be inspected or imported by hand.
- The app's own log is kept in the run's temp directory; the harness prints
  that path on failure.
- `shoot.js` deliberately avoids Playwright's `networkidle` wait — htmx keeps
  requests in flight, so it never settles and the run hangs instead of failing.
