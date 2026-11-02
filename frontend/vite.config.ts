import { defineConfig } from "vite";

// Dev/preview proxies so the globe can talk to the Go services without CORS:
//   /v1/compliance -> complianced (backend/cmd/complianced -listen :8090)
//   /v1            -> ingestd     (backend/cmd/ingestd -serve :8080)
// The more specific prefix must come first.
const proxy = {
  "/v1/compliance": "http://localhost:8090",
  "/v1": "http://localhost:8080",
};

export default defineConfig({
  server: { proxy },
  preview: { proxy },
  build: { target: "es2022", sourcemap: true },
});
