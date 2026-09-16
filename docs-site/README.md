# Pulse Pager docs site

The public documentation and pricing site, served on GitHub Pages. It is a
zero-build static site: plain HTML and CSS, with the API reference rendered in
the browser by Redoc from the OpenAPI spec.

## Pages

- `index.html` - overview, the open source vs cloud framing, and getting started.
- `api.html` - the API reference. Redoc (loaded from a CDN) renders
  `./openapi.yaml` in the browser.
- `guides/monitors.html` - what a monitor holds, how the plan floors the check
  interval, and how consecutive failures turn into an incident.
- `guides/authentication.html` - the API-key auth flow (`pulse_sk_` +
  `Authorization: Bearer`) and a "Webhooks (coming soon)" note.
- `pricing.html` - the cloud plan tiers (Free / Starter / Team / Business), with a
  self-host-for-free note up top, static.
- `assets/styles.css` - shared styles and the top nav.
- `assets/site.js` - the shared chrome, as two custom elements.

## Shared chrome

Every page gets the same folio strip, nav and page title from `<pulse-header>`,
and the same link grid from `<pulse-footer>`, so none of that markup is repeated
per page:

```html
<pulse-header active="pricing" base="./"
              eyebrow="Pulse Pager Cloud"
              heading="Cloud pricing"></pulse-header>
...
<pulse-footer base="./"></pulse-footer>
```

`active` highlights a nav item (overview, api, auth, pricing). `base` is the path
to the site root from that page, so it is `../` under `guides/`. `eyebrow` and
`heading` fill the masthead and both take inline HTML; leave them off for a page
that has no title block.

## How the API reference stays in sync

`api.html` loads `./openapi.yaml`, which is a copy of `api/openapi/v1.yaml` (the
single source of truth). The copy is the only build step:

```sh
make docs            # or: ./docs-site/build.sh
```

This copies `api/openapi/v1.yaml` to `docs-site/openapi.yaml`. CI re-runs it on
every push, so the published reference is always a build artifact of the spec and
cannot drift. `docs-site/openapi.yaml` is gitignored for that reason: it is
generated, never hand-edited.

The build needs no network. The Redoc script is fetched from a CDN only in the
browser, at view time.

## Preview locally

From the repo root:

```sh
make docs                          # copy the spec into docs-site/
cd docs-site
python3 -m http.server 8000        # or any static file server
```

Open http://localhost:8000. Serve over HTTP rather than opening the file
directly, so Redoc can fetch `./openapi.yaml`.

## How Pages deploys

`.github/workflows/docs.yml` runs on push to the default branch and on tags. It
runs `make docs` (re-copying the spec), uploads `docs-site/` as a Pages
artifact, and deploys with `actions/deploy-pages`. Enable Pages for the repo
with the source set to "GitHub Actions".
