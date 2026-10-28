# Wingman web UI

To use the browser UI, run `wingman server` in your project directory. See the
[main README](../../README.md#use-the-browser-ui) for setup.

## Build from source

Use Node.js 22.12+ (or a newer supported release). From this directory:

```bash
npm install
npm run build
```

The build writes assets to `../static`, which the Go server embeds. Rebuild
those assets before rebuilding the server or running browser tests.

From the repository root, `task run:web` builds the UI and starts the server.

Vite uses Oxc for React transforms, including automatic memoization through
`oxc-transform-react`. This native React Compiler integration is experimental;
keep its version compatible with `@vitejs/plugin-react` when updating dependencies.

Mermaid 12 previews require an ES2024-capable browser (Safari 17.4+). Diagram
renderers load on demand and use Mermaid's default layouts and look, with
light and dark color themes that follow the app.

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
