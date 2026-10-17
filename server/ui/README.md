# Wingman web UI

To use the browser UI, run `wingman server` in your project directory. See the
[main README](../../README.md#use-the-browser-ui) for setup.

## Build from source

From this directory:

```bash
npm install
npm run build
```

The build writes assets to `../static`, which the Go server embeds. Rebuild
those assets before rebuilding the server or running browser tests.

From the repository root, `task run:web` builds the UI and starts the server.

## Frontend development

Run a workspace server on port 4242 and start Vite from this directory:

```bash
# In a separate terminal at the repository root:
go run ./cmd/wingman server --port 4242 --no-browser

# In this directory:
npm run dev
```

Open the URL printed by Vite. It proxies `/api` requests and WebSocket
connections to the workspace server on port 4242. Configure the model provider
in the workspace server's environment. If you use a model gateway, run it on
a different port and set `WINGMAN_URL` to its address.

## Checks

```bash
npm run test:unit
npm run lint
npm run fmt:check
npm run build
```

For browser tests, install Chromium with `npx playwright install chromium`,
then run `npm run test:web` after building the assets. The browser fixture uses
a local fake model and needs no model account. Some editor checks also need
`gopls` installed. `E2E_GREP` selects tests by name.
