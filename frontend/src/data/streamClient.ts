/**
 * Live feed from the Go backend (backend/internal/api): loads recent journeys,
 * then follows the Server-Sent Events stream. EventSource reconnects on its
 * own; we report status changes to the caller.
 */
import type { PacketGlobe } from "../PacketGlobe";
import { journeyToPath, type PacketJourney } from "./journeyAdapter";

export type StreamStatus = "connecting" | "open" | "error" | "closed";

export interface StreamOptions {
  baseUrl?: string;
  /** recent journeys to load on connect */
  backfill?: number;
  onStatus?: (s: StreamStatus, detail?: string) => void;
  onJourney?: (j: PacketJourney) => void;
}

export function connectBackend(globe: PacketGlobe, opts: StreamOptions = {}): () => void {
  const base = (opts.baseUrl ?? "").replace(/\/$/, "");
  const status = opts.onStatus ?? (() => {});
  let closed = false;

  const ingest = (j: PacketJourney) => {
    const p = journeyToPath(j);
    if (p) globe.setPath(p);
    opts.onJourney?.(j);
  };

  status("connecting");
  fetch(`${base}/v1/journeys?limit=${opts.backfill ?? 200}`)
    .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
    .then((body: { journeys: PacketJourney[] }) => {
      if (!closed) body.journeys.slice().reverse().forEach(ingest);
    })
    .catch((err: Error) => status("error", `backfill: ${err.message}`));

  const es = new EventSource(`${base}/v1/stream`);
  es.onopen = () => status("open");
  es.onerror = () => status(es.readyState === EventSource.CLOSED ? "closed" : "error", "stream interrupted, retrying");
  es.addEventListener("journey", (ev) => {
    try {
      ingest(JSON.parse((ev as MessageEvent<string>).data) as PacketJourney);
    } catch (err) {
      console.warn("bad journey event", err);
    }
  });

  return () => {
    closed = true;
    es.close();
    status("closed");
  };
}
