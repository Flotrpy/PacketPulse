import { defineConfig } from "vite";

// The Go backend (backend/cmd/ingestd -serve :8080) is proxied in dev so the
// globe can use /v1/stream and /v1/journeys without CORS.
export default defineConfig({
  server: {
    proxy: { "/v1": "http://localhost:8080" },
  },
  build: { target: "es2022", sourcemap: true },
});
