import { defineConfig } from "vite";

// Dev/preview proxy so the globe talks to the Go backend without CORS.
// Default: the all-in-one server (backend/cmd/packetpulse) on :8080, which
// serves ingestion, compliance and the agent hub. To use the standalone
// services instead, set PACKETPULSE_COMPLIANCE=http://localhost:8090.
const api = process.env.PACKETPULSE_API ?? "http://localhost:8080";
const compliance = process.env.PACKETPULSE_COMPLIANCE ?? api;
const proxy = {
  "/v1/compliance": compliance, // the more specific prefix must come first
  "/v1": api,
};

export default defineConfig({
  server: { proxy },
  preview: { proxy },
  build: { target: "es2022", sourcemap: true },
});
