// @web/test-runner config (RFC-013 section 11). Runs the component and unit tests
// in a real browser (Lit web components want a real DOM, not jsdom). esbuild
// transpiles the TypeScript test files on the fly, so the tests are not part of
// the tsc typecheck pass.

import { esbuildPlugin } from "@web/dev-server-esbuild";
import { fileURLToPath } from "node:url";

// Point esbuild at our tsconfig so it uses experimentalDecorators +
// useDefineForClassFields:false. Without it esbuild applies TC39 (standard)
// decorator semantics, which Lit's @property/@state decorators reject with
// "Unsupported decorator location: field".
const tsconfig = fileURLToPath(new URL("./tsconfig.json", import.meta.url));

export default {
  files: "src/**/*.test.ts",
  // Prefer the "browser" export condition so deps with separate node/browser builds
  // (e.g. the OpenTelemetry packages, RFC-021 phase 2) resolve to the browser build,
  // which uses Web Crypto instead of node's crypto/util built-ins.
  nodeResolve: {
    browser: true,
    exportConditions: ["browser", "module", "import", "default"],
  },
  // TanStack table-core reads process.env.NODE_ENV for dev checks. Vite replaces
  // it at build time; in the browser test page we provide a process shim so the
  // reference resolves (Node-style global is otherwise absent in the browser).
  testRunnerHtml: (testFramework) => `<!doctype html>
    <html>
      <head><script>window.process = { env: { NODE_ENV: "development" } };</script></head>
      <body><script type="module" src="${testFramework}"></script></body>
    </html>`,
  // Run one file at a time. The focus-management tests (confirm-dialog) rely on
  // real document focus, which only behaves on the browser page that currently
  // holds OS focus; running files concurrently leaves other pages unfocused and
  // makes those tests flake/time out.
  concurrency: 1,
  plugins: [
    esbuildPlugin({
      ts: true,
      target: "es2022",
      tsconfig,
      // TanStack table-core (and some deps) read process.env.NODE_ENV for dev
      // checks; Vite replaces it at build time, so mirror that for the test bundle.
      // import.meta.env.DEV is Vite's dev flag (login-view gates its dev sign-in on
      // it); Vite injects it in real builds, so define it true for the test bundle.
      define: {
        "process.env.NODE_ENV": '"development"',
        "import.meta.env.DEV": "true",
      },
    }),
  ],
};
