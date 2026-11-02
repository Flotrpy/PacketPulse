/**
 * Demo shell around PacketGlobe.
 *   ?demo=500      simulated load (default 300)
 *   ?example=1     the backend's example journey (Mumbai -> Frankfurt)
 *   ?live=1        follow the Go backend's /v1/stream (vite proxies /v1 to :8080)
 */
import { PacketGlobe } from "./PacketGlobe";
import { statusOf } from "./core/health";
import type { HopSelection } from "./core/types";
import { journeyToPath, type PacketJourney } from "./data/journeyAdapter";
import { Simulator } from "./data/simulator";
import { connectBackend } from "./data/streamClient";

const $ = <T extends HTMLElement>(sel: string) => document.querySelector(sel) as T;
const container = $("#globe");
const statusEl = $("#status");
const inspector = $("#inspector");

const globe = new PacketGlobe({ container, landUrl: `${import.meta.env.BASE_URL}data/land.json` });
(window as unknown as { packetGlobe: PacketGlobe }).packetGlobe = globe; // handy for console debugging

let sim: Simulator | null = null;
let simTimer = 0;
let stopLive: (() => void) | null = null;

function stopSources(): void {
  if (simTimer) clearInterval(simTimer);
  simTimer = 0;
  sim = null;
  stopLive?.();
  stopLive = null;
  globe.clear();
  document.querySelectorAll("#hud button").forEach((b) => b.classList.remove("active"));
}

function startDemo(n: number): void {
  stopSources();
  sim = new Simulator({ seed: 7 });
  sim.spawn(n).forEach((p) => globe.setPath(p));
  const t0 = performance.now();
  let last = t0;
  simTimer = window.setInterval(() => {
    const now = performance.now();
    const { updated, removed } = sim!.step((now - t0) / 1000, (now - last) / 1000);
    last = now;
    removed.forEach((id) => globe.removePath(id));
    updated.forEach((p) => globe.setPath(p));
  }, 250);
  document.querySelector(`[data-demo="${n}"]`)?.classList.add("active");
  statusEl.textContent = `simulating ${n} concurrent paths`;
}

async function loadExample(): Promise<void> {
  stopSources();
  $("#b-example").classList.add("active");
  const res = await fetch(`${import.meta.env.BASE_URL}data/example-journey.json`);
  const path = journeyToPath((await res.json()) as PacketJourney);
  if (!path) return;
  globe.setPath(path);
  statusEl.textContent = `example: ${path.label} (${path.verdict})`;
  await globe.focusPath(path.id);
}

function startLive(): void {
  stopSources();
  $("#b-live").classList.add("active");
  stopLive = connectBackend(globe, {
    onStatus: (s, detail) => (statusEl.textContent = `backend: ${s}${detail ? ` – ${detail}` : ""}`),
  });
}

// ---------- inspector ----------

function esc(s: unknown): string {
  return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]!);
}

function renderInspector(sel: HopSelection | null): void {
  if (!sel) {
    inspector.classList.remove("open");
    return;
  }
  const path = globe.getPath(sel.pathId);
  const h = sel.hop;
  const st = statusOf(sel.severity);
  const where = [h.city, h.country].filter(Boolean).join(", ") || `${h.lat.toFixed(2)}, ${h.lon.toFixed(2)}`;
  inspector.innerHTML = `
    <h2>${esc(path?.label ?? sel.pathId)}</h2>
    <dl>
      <dt>hop</dt><dd>${sel.hopIndex} / ${(path?.hops.length ?? 1) - 1} ${h.hopNumber !== undefined ? `(TTL ${h.hopNumber})` : ""}</dd>
      <dt>where</dt><dd>${esc(where)}</dd>
      ${h.ip ? `<dt>ip</dt><dd>${esc(h.ip)}</dd>` : ""}
      ${h.hostname ? `<dt>rdns</dt><dd>${esc(h.hostname)}</dd>` : ""}
      ${h.asn ? `<dt>asn</dt><dd>AS${h.asn} ${esc((h.asOrg ?? "").replace(/^AS\d+\s*-\s*/, ""))}</dd>` : ""}
      ${h.medium ? `<dt>link in</dt><dd>${esc(h.medium.replace(/_/g, " "))}${h.cable ? ` · ${esc(h.cable)}` : ""}</dd>` : ""}
      <dt>rtt</dt><dd>${h.rttMs.toFixed(1)} ms</dd>
      <dt>jitter</dt><dd>${(h.jitterMs ?? 0).toFixed(1)} ms</dd>
      <dt>loss</dt><dd>${(h.lossPct ?? 0).toFixed(1)} %</dd>
      <dt>health</dt><dd>${sel.hopIndex === 0 ? "source" : `<span class="pill ${st}">${st}</span> latency ×${sel.latencyExcess.toFixed(2)} of light-speed`}</dd>
      ${h.borderViolation ? `<dt>policy</dt><dd><span class="pill red">border violation</span></dd>` : ""}
    </dl>
    <div class="hops">${(path?.hops ?? []).map((_, i) => `<button data-hop="${i}" class="${i === sel.hopIndex ? "active" : ""}">${i}</button>`).join("")}</div>`;
  inspector.classList.add("open");
  inspector.querySelectorAll<HTMLButtonElement>("[data-hop]").forEach((b) =>
    b.addEventListener("click", () => void globe.focusHop(sel.pathId, Number(b.dataset.hop))),
  );
}

// Re-rendering on every live update would steal clicks; throttle to 4/s.
let pending: HopSelection | null | undefined;
globe.onSelect((sel) => {
  if (pending === undefined) setTimeout(() => { renderInspector(pending ?? null); pending = undefined; }, 250);
  pending = sel;
});

// ---------- HUD ----------

setInterval(() => {
  const s = globe.stats();
  $("#s-fps").textContent = String(s.fps);
  $("#s-paths").textContent = String(s.paths);
  $("#s-segs").textContent = String(s.segments);
  $("#s-calls").textContent = String(s.drawCalls);
}, 500);

document.querySelectorAll<HTMLButtonElement>("[data-demo]").forEach((b) => b.addEventListener("click", () => startDemo(Number(b.dataset.demo))));
$("#b-example").addEventListener("click", () => void loadExample());
$("#b-live").addEventListener("click", startLive);
$("#b-clear").addEventListener("click", () => {
  stopSources();
  statusEl.textContent = "";
});
window.addEventListener("keydown", (e) => {
  if (e.key === "Escape") globe.clearSelection();
});

const q = new URLSearchParams(location.search);
if (q.has("live")) startLive();
else if (q.has("example")) void loadExample();
else startDemo(Number(q.get("demo")) || 300);
