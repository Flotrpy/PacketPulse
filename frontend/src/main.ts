/**
 * Demo shell around PacketGlobe.
 *   (default)      live: your traces (ppagent) and other journeys from the backend
 *   ?demo=500      simulated load
 *   ?example=1     the backend's example journey (Mumbai -> Frankfurt)
 *   ?live=1        follow the Go backend's /v1/stream (vite proxies /v1 to :8080)
 *   ?business=1    start in Business Mode (needs complianced; vite proxies /v1/compliance to :8090)
 *   &class=...     data classification to evaluate in Business Mode (class=tags: each path's ingestion tags;
 *                  the default with ?live=1)
 */
import { PacketGlobe } from "./PacketGlobe";
import { statusOf } from "./core/health";
import type { HopSelection } from "./core/types";
import { inframapToPath, type InfraMap } from "./data/inframapAdapter";
import { BusinessMode, TAGS } from "./business/BusinessMode";
import { complianceCss, statusCode } from "./core/compliance";
import { Simulator } from "./data/simulator";
import { connectBackend } from "./data/streamClient";
import { TracePanel } from "./agent/TracePanel";

const $ = <T extends HTMLElement>(sel: string) => document.querySelector(sel) as T;
const container = $("#globe");
const statusEl = $("#status");
const inspector = $("#inspector");

const globe = new PacketGlobe({ container, landUrl: `${import.meta.env.BASE_URL}data/land.json` });
(window as unknown as { packetGlobe: PacketGlobe }).packetGlobe = globe; // handy for console debugging
const business = new BusinessMode(globe, $("#business"));
(window as unknown as { business: BusinessMode }).business = business;

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
  document.querySelectorAll("#hud button:not([data-mode])").forEach((b) => b.classList.remove("active"));
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
  // The enriched InfraMap of the Jio 5G (Mumbai) -> AWS Frankfurt trace: it
  // carries the subsea cable's landing stations, which Business Mode checks.
  const res = await fetch(`${import.meta.env.BASE_URL}data/example-inframap.json`);
  const path = inframapToPath((await res.json()) as InfraMap);
  if (!path) return;
  globe.setPath(path);
  statusEl.textContent = `example: Jio Mumbai → ${path.label} (enriched trace)`;
  await globe.focusPath(path.id);
}

function startLive(): void {
  stopSources();
  $("#b-live").classList.add("active");
  stopLive = connectBackend(globe, {
    onStatus: (s, detail) => (statusEl.textContent = `backend: ${s}${detail ? ` – ${detail}` : ""}`),
  });
}

// Traces from the user's own computer (ppagent) arrive on the live stream.
new TracePanel($("#trace"), globe, () => {
  if (!stopLive) startLive();
});

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
      ${complianceRows(sel)}
    </dl>
    <div class="hops">${(path?.hops ?? []).map((_, i) => `<button data-hop="${i}" class="${i === sel.hopIndex ? "active" : ""}">${i}</button>`).join("")}</div>`;
  inspector.classList.add("open");
  inspector.querySelectorAll<HTMLButtonElement>("[data-hop]").forEach((b) =>
    b.addEventListener("click", () => void globe.focusHop(sel.pathId, Number(b.dataset.hop))),
  );
}

/** Business Mode rows: jurisdiction of the hop and what the engine said about it. */
function complianceRows(sel: HopSelection): string {
  if (!business.isEnabled) return "";
  const d = business.hopDetails(sel.pathId, sel.hopIndex);
  if (d?.verdict === "untagged") return `<dt>data class</dt><dd class="muted">untagged at ingestion: no profile applies</dd>`;
  if (!d?.point) return `<dt>compliance</dt><dd class="muted">not evaluated yet</dd>`;
  const p = d.point;
  const code = statusCode(p.status);
  const where = [p.location.subdivision_name, p.location.country_name ?? p.location.country_iso2].filter(Boolean).join(", ") || "international waters";
  let rows = `<dt>data class</dt><dd>${esc(d.labels.join(", ").replace(/_/g, " "))}</dd>
    <dt>jurisdiction</dt><dd>${esc(where)}${p.regions.length ? ` <span class="muted">(${esc(p.regions.join(", "))})</span>` : ""}</dd>
    <dt>compliance</dt><dd><span class="pill" style="color:${complianceCss(code)};background:rgba(255,255,255,0.06)">${esc(p.status.replace("_", " "))}</span>${p.flagged_by?.length ? ` <span class="muted">${esc(p.flagged_by.join(", "))}</span>` : ""}</dd>`;
  for (const v of d.alerts) {
    rows += `<dt>alert</dt><dd>${esc(v.alert.law.code)} · entered ${esc(new Date(v.alert.entered_at).toISOString().slice(11, 23))} UTC<br><span class="muted">${esc(v.alert.mitigation.recommendation)}</span></dd>`;
  }
  return rows;
}

// Re-rendering on every live update would steal clicks; throttle to 4/s.
let pending: HopSelection | null | undefined;
globe.onSelect((sel) => {
  business.onSelect(sel);
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
const businessBtn = $("#b-business");
const legend = $("#legend");
const latencyLegend = legend.innerHTML;
const businessLegend = ["violation", "suspected", "compliant", "not_evaluated"]
  .map((s) => `<span><i style="display:inline-block;width:9px;height:9px;border-radius:50%;margin-right:4px;background:${complianceCss(statusCode(s))}"></i>${s.replace("_", " ")}</span>`)
  .join("");
async function toggleBusiness(on: boolean): Promise<void> {
  businessBtn.classList.toggle("active", on);
  legend.innerHTML = on ? businessLegend : latencyLegend;
  if (on) await business.enable();
  else business.disable();
}
businessBtn.addEventListener("click", () => void toggleBusiness(!business.isEnabled));
$("#b-live").addEventListener("click", startLive);
$("#b-clear").addEventListener("click", () => {
  stopSources();
  statusEl.textContent = "";
});
window.addEventListener("keydown", (e) => {
  if (e.key === "Escape") globe.clearSelection();
});

const q = new URLSearchParams(location.search);
if (q.has("business")) {
  const cls = q.get("class") ?? (q.has("demo") || q.has("example") ? "" : "tags");
  if (cls) void business.setClassification(cls === "tags" ? TAGS : cls);
  void toggleBusiness(true);
}
// Default: live traces (yours, from ppagent). ?demo=N simulates, ?example=1 shows the recorded journey.
if (q.has("example")) void loadExample();
else if (q.has("demo")) startDemo(Number(q.get("demo")) || 300);
else startLive();
