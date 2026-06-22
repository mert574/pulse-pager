// CI bundle-budget check (RFC-013 section 2.2, decision D9). Fails the build if a
// built chunk grows past its gzipped budget, so bloat is caught at the PR, not in
// production. Run after `vite build`, against dist/.
//
//   app initial entry (shell + first route): <= 60 KB
//   any lazy route chunk:                     <= 40 KB
//   status public entry (whole page):         <= 30 KB
//
// These are starting budgets; adjust here when a deliberate, reviewed increase is
// agreed. Bundles are matched by Vite's content-hashed filenames.

import { readdirSync, readFileSync, statSync } from "node:fs";
import { gzipSync } from "node:zlib";
import { join } from "node:path";

const KB = 1024;
const ASSETS = new URL("../dist/assets/", import.meta.url).pathname;

// budget in KB (gzipped), matched against the entry/chunk base name
const BUDGETS = [
  { test: (n) => n.startsWith("app") && n.endsWith(".js"), budget: 60, label: "app entry" },
  { test: (n) => n.startsWith("status") && n.endsWith(".js"), budget: 30, label: "status entry" },
  // every other js chunk is a lazy route chunk
  { test: (n) => n.endsWith(".js"), budget: 40, label: "route chunk" },
  // Tailwind utilities used + the two daisyUI themes (RFC-013 section 2.2). One
  // shared sheet is emitted for both entries, so a single budget covers it.
  { test: (n) => n.endsWith(".css"), budget: 40, label: "stylesheet" },
];

function gzipKB(path) {
  const raw = readFileSync(path);
  return gzipSync(raw).length / KB;
}

function matchBudget(name) {
  return BUDGETS.find((b) => b.test(name));
}

let files;
try {
  files = readdirSync(ASSETS).filter(
    (f) => f.endsWith(".js") || f.endsWith(".css"),
  );
} catch {
  console.error(`no built assets found at ${ASSETS}; run "vite build" first`);
  process.exit(1);
}

let failed = false;
for (const name of files.sort()) {
  const path = join(ASSETS, name);
  if (!statSync(path).isFile()) continue;
  const size = gzipKB(path);
  const rule = matchBudget(name);
  const budget = rule?.budget ?? Infinity;
  const ok = size <= budget;
  if (!ok) failed = true;
  const tag = ok ? "ok " : "OVER";
  console.log(
    `${tag}  ${name}  ${size.toFixed(1)} KB gz  (${rule?.label ?? "?"}, budget ${budget} KB)`,
  );
}

if (failed) {
  console.error("\nbundle budget exceeded");
  process.exit(1);
}
console.log("\nall chunks within budget");

// Sentinel: a known i18n value must be present SOMEWHERE in the built JS. If it
// is in no chunk, the i18n module was not bundled, almost always because a stray
// compiled src/*.js shadowed the .ts source for the bundler (it resolves a
// "./x.js" import to the literal file over x.ts). tsc and the test runner can
// stay green while the real app renders raw keys, so we catch it here. We scan
// all chunks because code-splitting may hoist the shared i18n module into any of
// them (e.g. a chunk shared by the app and status entries).
const SENTINEL = "Sign in to your Pulse account";
const present = readdirSync(ASSETS)
  .filter((f) => f.endsWith(".js"))
  .some((f) => readFileSync(join(ASSETS, f), "utf8").includes(SENTINEL));
if (!present) {
  console.error(
    `\ni18n values missing from the build (sentinel "${SENTINEL}" found in no chunk).\n` +
      "Likely a stray compiled .js under src/ shadowing a .ts module. " +
      "Check `find src -name '*.js'`.",
  );
  process.exit(1);
}
console.log("i18n sentinel present");
