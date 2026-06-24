import { defineConfig } from "vite";
import { resolve } from "node:path";
import tailwindcss from "@tailwindcss/vite";

// Pulse can run behind a reverse proxy at a sub-path (PULSE_BASE_PATH). For the
// build we default base to "/". When the operator sets a base path, the server
// rewrites the <base href> in index.html at serve time, and the router reads that
// href at runtime. So the built asset URLs stay relative and keep working.
//
// Two entries (RFC-013 section 2.2, 8.1):
//   - index.html: the authed SPA (shell + lazy route chunks)
//   - status.html: the public status page, a separate lightweight bundle that
//     never imports the authed app, served unauthenticated and cache-first.
//
// Dev server proxies /api and /auth (OAuth + refresh) and /healthz to the Go
// backend so the SPA can talk to it with HMR and no binary rebuild.
export default defineConfig({
  base: "/",
  // Read .env from the repo root (one level up) instead of web/, so the build picks
  // up the same root .env the Go services use. Only VITE_-prefixed vars are exposed
  // to the client bundle, so the PULSE_* secrets in that file stay server-side.
  envDir: resolve(__dirname, ".."),
  plugins: [tailwindcss()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    rollupOptions: {
      input: {
        app: resolve(__dirname, "index.html"),
        status: resolve(__dirname, "status.html"),
        admin: resolve(__dirname, "admin.html"),
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://localhost:8081", changeOrigin: false },
      "/auth": { target: "http://localhost:8081", changeOrigin: false },
      "/healthz": { target: "http://localhost:8081", changeOrigin: false },
    },
  },
});
