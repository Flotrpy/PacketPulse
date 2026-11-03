/**
 * Business Mode: evaluates every path on the globe against the compliance
 * engine and renders the result.
 *
 *   - jurisdiction overlay: permitted regions teal, prohibited regions red
 *   - arcs and hops recoloured by compliance status (violations flash)
 *   - an alert marker at every territory entry, with a clickable list
 *   - Law Directory tab for the territories the traffic touches
 *
 * Evaluations use the engine's preview mode (no alerts published): the globe
 * is a viewer, alert publishing belongs to the ingestion pipeline. Only new or
 * rerouted paths are re-evaluated; latency-only updates cannot change
 * compliance. Registry changes made by admins are picked up by "Refresh".
 *
 * Data class: "per-path tags" (TAGS) evaluates each path with the
 * classifications assigned at ingestion; untagged paths are not governed by
 * any profile and stay grey. Picking a class instead evaluates every path as
 * if it carried that class (a what-if view).
 */
import { complianceCss } from "../core/compliance";
import type { HopSelection } from "../core/types";
import type { PacketGlobe, PathChange } from "../PacketGlobe";
import type { GeoJSONFeatureCollection } from "../render/Earth";
import { ComplianceClient, type LawEntry, type Profile } from "./client";
import { applyEvaluation, buildPacket, sortAlerts, type AlertView, type ResolvedPoint, type SeqRef } from "./packet";

const BATCH = 200;
/** Data-class selector value: use each path's own ingestion tags. */
export const TAGS = "@tags";
const MAX_MARKERS = 2000;
const SEVERITY_RGB: Record<string, [number, number, number]> = {
  critical: [1.0, 0.12, 0.14], high: [1.0, 0.35, 0.12], medium: [1.0, 0.65, 0.1], low: [1.0, 0.85, 0.3], info: [0.4, 0.75, 1.0],
};

interface PathState {
  verdict: string;
  alerts: AlertView[];
  hopPoints: ResolvedPoint[];
}

export class BusinessMode {
  private enabled = false;
  private classification = "";
  private profiles: Profile[] = [];
  private state = new Map<string, PathState>();
  private geometryCache = new Map<string, GeoJSONFeatureCollection>();
  private lawCache = new Map<string, { as_of: string; jurisdictions: LawEntry[]; not_in_directory: string[] }>();
  private generation = 0; // bumps on classification/refresh; stale responses are dropped
  private unsubscribe?: () => void;
  private tab: "alerts" | "laws" = "alerts";
  private selectedPath: string | null = null;
  private lastError = "";

  constructor(
    private globe: PacketGlobe,
    private root: HTMLElement,
    private client = new ComplianceClient(),
  ) {
    this.root.addEventListener("click", (e) => this.onClick(e));
    this.root.addEventListener("change", (e) => {
      const t = e.target as HTMLSelectElement;
      if (t.id === "bm-class") void this.setClassification(t.value);
    });
  }

  get isEnabled(): boolean {
    return this.enabled;
  }

  async enable(): Promise<void> {
    if (this.enabled) return;
    this.enabled = true;
    this.root.classList.add("open");
    this.globe.setBusinessMode(true);
    this.unsubscribe = this.globe.onPathsChanged((c) => void this.onPathsChanged(c));
    await this.refresh();
  }

  disable(): void {
    if (!this.enabled) return;
    this.enabled = false;
    this.generation++;
    this.unsubscribe?.();
    this.globe.setBusinessMode(false);
    this.root.classList.remove("open");
  }

  /** Reload profiles (picks up admin changes) and re-evaluate everything. */
  async refresh(): Promise<void> {
    try {
      this.status("loading compliance profiles…");
      this.profiles = (await this.client.profiles()).profiles.filter((p) => p.enabled);
      const classes = this.classifications();
      let note = "";
      if (this.classification !== TAGS && !classes.includes(this.classification)) {
        const prev = this.classification;
        this.classification = classes.includes("sensitive_eu_citizen_data") ? "sensitive_eu_citizen_data" : (classes[0] ?? "");
        if (prev) note = `"${prev.replace(/_/g, " ")}" is no longer governed by an enabled profile; showing "${this.classification.replace(/_/g, " ")}".`;
      }
      this.lastError = note;
      await this.setClassification(this.classification, true);
    } catch (err) {
      this.fail(err);
    }
  }

  async setClassification(c: string, force = false): Promise<void> {
    if (!this.enabled) {
      this.classification = c; // applied on enable()
      return;
    }
    if (!force && c === this.classification) return;
    this.classification = c;
    this.generation++;
    this.state.clear();
    this.globe.clearCompliance();
    this.render();
    try {
      await this.loadOverlay();
      await this.evaluate(this.globe.pathIds);
    } catch (err) {
      this.fail(err);
    }
  }

  /** Labels a path is evaluated with in the current mode. */
  labelsFor(path: { classifications?: string[] }): string[] {
    return this.classification === TAGS ? (path.classifications ?? []) : this.classification ? [this.classification] : [];
  }

  /** Labels across the paths on the globe (TAGS mode) or the picked class. */
  private activeLabels(): Set<string> {
    if (this.classification !== TAGS) return new Set(this.classification ? [this.classification] : []);
    const out = new Set<string>();
    for (const id of this.globe.pathIds) this.globe.getPath(id)?.classifications?.forEach((l) => out.add(l));
    return out;
  }

  /** Profiles that govern the active labels. */
  activeProfiles(): Profile[] {
    const labels = this.activeLabels();
    return this.profiles.filter((p) => p.classifications.some((c) => labels.has(c)));
  }

  private classifications(): string[] {
    return [...new Set(this.profiles.flatMap((p) => p.classifications))].sort();
  }

  /** Permitted = allow-list regions + "allow" exceptions; prohibited = deny-list regions. */
  private overlayKey = "";

  private async loadOverlay(): Promise<void> {
    this.overlayKey = [...this.activeLabels()].sort().join(",");
    const permitted = new Set<string>();
    const prohibited = new Set<string>();
    for (const p of this.activeProfiles()) {
      if (p.mode === "allow_list") {
        p.regions.forEach((r) => permitted.add(r));
        p.exceptions?.filter((e) => e.effect === "allow").forEach((e) => permitted.add(e.region));
      } else {
        p.regions.forEach((r) => prohibited.add(r));
      }
    }
    const gen = this.generation;
    const fetchAll = (ids: Set<string>) => Promise.all([...ids].map((id) => this.geometry(id)));
    const [a, b] = await Promise.all([fetchAll(permitted), fetchAll(prohibited)]);
    if (gen === this.generation) this.globe.setRegionOverlay(a, b);
  }

  private async geometry(id: string): Promise<GeoJSONFeatureCollection> {
    const hit = this.geometryCache.get(id);
    if (hit) return hit;
    const fc = await this.client.regionGeometry(id);
    this.geometryCache.set(id, fc);
    return fc;
  }

  private async onPathsChanged(c: PathChange): Promise<void> {
    if (!this.enabled) return;
    let changed = false;
    for (const id of c.removed) changed = this.state.delete(id) || changed;
    if (changed) this.paint();
    // New tags on the globe bring new profiles: redraw their jurisdictions.
    if (this.classification === TAGS && [...this.activeLabels()].sort().join(",") !== this.overlayKey) await this.loadOverlay().catch((err) => this.fail(err));
    if (c.rerouted.length) await this.evaluate(c.rerouted).catch((err) => this.fail(err));
  }

  private async evaluate(ids: string[]): Promise<void> {
    if (!this.classification || ids.length === 0) {
      this.paint();
      return;
    }
    const gen = this.generation;
    const start = new Date();
    for (let i = 0; i < ids.length; i += BATCH) {
      const chunk: { id: string; index: SeqRef[]; packet: ReturnType<typeof buildPacket>["packet"] }[] = [];
      for (const id of ids.slice(i, i + BATCH)) {
        const path = this.globe.getPath(id);
        if (!path) continue;
        const labels = this.labelsFor(path);
        if (!labels.length) {
          // Untagged: no profile applies, nothing to ask the engine.
          this.state.set(id, { verdict: "untagged", alerts: [], hopPoints: [] });
          continue;
        }
        const t0 = path.startedAt ? new Date(path.startedAt) : start; // live/simulated paths: now
        const { packet, index } = buildPacket(path, labels, Number.isNaN(t0.getTime()) ? start : t0);
        chunk.push({ id, index, packet });
      }
      if (!chunk.length) continue;
      this.status(`evaluating ${Math.min(i + BATCH, ids.length)}/${ids.length} paths…`);
      const res = await this.client.evaluateBatch(chunk.map((c) => c.packet));
      if (gen !== this.generation || !this.enabled) return; // classification changed meanwhile
      const byId = new Map(chunk.map((c) => [c.id, c]));
      for (const ev of res.evaluations) {
        const c = byId.get(ev.packet_id);
        const path = c && this.globe.getPath(c.id);
        if (!c || !path || path.hops.length !== c.index.filter((r) => r.landing === undefined).length) continue; // rerouted meanwhile
        const applied = applyEvaluation(path, ev, c.index);
        this.globe.setPathCompliance(c.id, applied.compliance);
        this.state.set(c.id, { verdict: ev.verdict, alerts: applied.alerts, hopPoints: applied.hopPoints });
      }
      if (res.errors.length) this.lastError = `${res.errors.length} packet(s) rejected: ${res.errors[0].error}`;
    }
    this.paint();
  }

  // ---------------- rendering ----------------

  private allAlerts(): AlertView[] {
    const out: AlertView[] = [];
    for (const s of this.state.values()) out.push(...s.alerts);
    return out;
  }

  private paint(): void {
    const alerts = sortAlerts(this.allAlerts());
    this.globe.setAlertMarkers(
      alerts.slice(0, MAX_MARKERS).map((v) => ({
        lat: v.alert.territory.lat,
        lon: v.alert.territory.lon,
        rgb: SEVERITY_RGB[v.alert.severity] ?? SEVERITY_RGB.high,
        suspected: v.alert.suspected || !!v.alert.exception,
      })),
    );
    this.render();
    if (this.tab === "laws") void this.renderLaws();
  }

  private status(msg: string): void {
    const el = this.root.querySelector("#bm-status");
    if (el) el.textContent = msg;
  }

  private fail(err: unknown): void {
    this.lastError = err instanceof Error ? err.message : String(err);
    this.render();
  }

  private counts() {
    const c = { compliant: 0, violation: 0, suspected_violation: 0, not_applicable: 0, untagged: 0 } as Record<string, number>;
    for (const s of this.state.values()) c[s.verdict] = (c[s.verdict] ?? 0) + 1;
    return c;
  }

  private render(): void {
    if (!this.enabled) return;
    const classes = this.classifications();
    const profiles = this.activeProfiles();
    const c = this.counts();
    const alerts = sortAlerts(this.allAlerts());
    const evaluated = this.state.size;
    this.root.innerHTML = `
      <div class="bm-head">
        <h2>Business Mode</h2>
        <button data-act="refresh" title="Reload profiles and re-evaluate">Refresh</button>
      </div>
      <label class="bm-row">Data class
        <select id="bm-class"><option value="${TAGS}" ${this.classification === TAGS ? "selected" : ""}>per-path tags (from ingestion)</option>${classes.map((x) => `<option value="${esc(x)}" ${x === this.classification ? "selected" : ""}>${esc(x.replace(/_/g, " "))}</option>`).join("")}</select>
      </label>
      <div class="bm-profiles">${profiles.map((p) => `<div><b>${esc(p.law.code)}</b> ${esc(p.name)} <span class="muted">(${p.mode === "allow_list" ? "permitted" : "prohibited"}: ${esc(p.regions.join(", "))}; ${p.scope.replace("_", " ")})</span></div>`).join("") || `<div class="muted">${this.classification === TAGS ? (this.activeLabels().size ? `No enabled profile governs ${esc([...this.activeLabels()].join(", "))}.` : "No path on the globe carries ingestion tags yet.") : "No enabled profile governs this class."}</div>`}</div>
      ${this.classification === TAGS && this.activeLabels().size ? `<div class="muted">Tags on the globe: ${esc([...this.activeLabels()].sort().join(", ").replace(/_/g, " "))}</div>` : ""}
      <div class="bm-counts">
        <span><i style="background:${complianceCss(3)}"></i>${c.violation} violating</span>
        <span><i style="background:${complianceCss(2)}"></i>${c.suspected_violation} suspected</span>
        <span><i style="background:${complianceCss(1)}"></i>${c.compliant} compliant</span>
        ${c.untagged ? `<span class="muted">${c.untagged} untagged</span>` : ""}
        <span class="muted">${evaluated - c.untagged}/${this.globe.pathIds.length} evaluated</span>
      </div>
      <div id="bm-status" class="${this.lastError ? "err" : "muted"}">${esc(this.lastError)}</div>
      <div class="bm-tabs">
        <button data-tab="alerts" class="${this.tab === "alerts" ? "active" : ""}">Alerts (${alerts.length})</button>
        <button data-tab="laws" class="${this.tab === "laws" ? "active" : ""}">Law Directory</button>
      </div>
      <div class="bm-body">${this.tab === "alerts" ? this.alertsHtml(alerts) : '<div id="bm-laws" class="muted">loading…</div>'}</div>`;
  }

  private alertsHtml(alerts: AlertView[]): string {
    if (!alerts.length) return `<div class="muted">${this.state.size ? "No violations on the evaluated paths." : "Waiting for evaluations…"}</div>`;
    const shown = this.selectedPath ? alerts.filter((a) => a.pathId === this.selectedPath) : alerts;
    const head = this.selectedPath ? `<div class="muted">Showing the selected path · <a href="#" data-act="all">show all</a></div>` : "";
    return (
      head +
      shown
        .slice(0, 100)
        .map((v, i) => {
          const a = v.alert;
          const where = [a.territory.subdivision_name, a.territory.country_name ?? a.territory.country_iso2].filter(Boolean).join(", ");
          const path = this.globe.getPath(v.pathId);
          const review = a.suspected || !!a.exception;
          return `<div class="bm-alert ${review ? "suspected" : ""}" data-alert="${i}">
            <div><span class="sev ${a.severity}">${a.severity}${a.suspected ? " · suspected" : a.exception ? " · review" : ""}</span> <b>${esc(where)}</b> <span class="muted">${esc(a.point.kind.replace("_", " "))}</span></div>
            <div class="muted">${esc(path?.label ?? v.pathId)}${this.classification === TAGS && path?.classifications?.length ? ` [${esc(path.classifications.join(", "))}]` : ""} · entered ${esc(a.entered_at.replace("T", " ").replace("Z", " UTC"))}</div>
            <div>${esc(a.law.code)}: ${esc(a.law.citation)}</div>
            ${a.exception ? `<div class="muted">${esc(a.exception)}</div>` : ""}
            <div class="rec">${esc(a.mitigation.recommendation)}</div>
          </div>`;
        })
        .join("") +
      (shown.length > 100 ? `<div class="muted">+${shown.length - 100} more</div>` : "")
    );
  }

  /** Law Directory: territories of the selected path, else of all alerts. */
  private async renderLaws(): Promise<void> {
    const countries = new Set<string>();
    const sel = this.selectedPath && this.state.get(this.selectedPath);
    if (sel) {
      sel.hopPoints.forEach((p) => p?.location.country_iso2 && countries.add(p.location.country_iso2));
      sel.alerts.forEach((a) => countries.add(a.alert.territory.country_iso2));
    } else {
      this.allAlerts().forEach((a) => countries.add(a.alert.territory.country_iso2));
    }
    const list = [...countries].sort().slice(0, 40);
    const el = () => this.root.querySelector("#bm-laws");
    if (!list.length) {
      const e = el();
      if (e) e.innerHTML = "Select a path or wait for alerts: the directory lists the laws of the territories the traffic touches.";
      return;
    }
    const key = list.join(",");
    try {
      let data = this.lawCache.get(key);
      if (!data) {
        data = await this.client.laws(list);
        this.lawCache.set(key, data);
      }
      const e = el();
      if (!e || this.tab !== "laws") return;
      e.classList.remove("muted");
      e.innerHTML =
        `<div class="muted">${sel ? "Territories on the selected path" : "Territories with alerts"} · directory as of ${esc(data.as_of)} · informational, not legal advice</div>` +
        data.jurisdictions
          .map(
            (j) => `<div class="bm-law">
              <div><b>${esc(j.name)}</b> ${j.localization ? `<span class="pill loc-${esc(j.localization.level)}">localisation: ${esc(j.localization.level)}</span>` : ""} ${j.transfer_regime ? `<span class="muted">${esc(j.transfer_regime.replace(/_/g, " "))}</span>` : ""}</div>
              <ul>${j.laws.map((l) => `<li><b>${esc(l.short)}</b> ${esc(l.name)} <span class="muted">${esc(l.citation)}${l.in_force ? ` · ${esc(l.in_force)}` : ""}</span></li>`).join("")}</ul>
              ${j.dla_piper_url ? `<a href="${esc(j.dla_piper_url)}" target="_blank" rel="noopener">DLA Piper summary ↗</a>` : ""}
            </div>`,
          )
          .join("") +
        (data.not_in_directory.length ? `<div class="muted">Not in the directory yet: ${esc(data.not_in_directory.join(", "))}</div>` : "");
    } catch (err) {
      const e = el();
      if (e) e.textContent = err instanceof Error ? err.message : String(err);
    }
  }

  private onClick(e: Event): void {
    const t = e.target as HTMLElement;
    const act = t.closest<HTMLElement>("[data-act]")?.dataset.act;
    if (act === "refresh") {
      this.geometryCache.clear();
      void this.refresh();
      return;
    }
    if (act === "all") {
      e.preventDefault();
      this.selectedPath = null;
      this.render();
      return;
    }
    const tab = t.closest<HTMLElement>("[data-tab]")?.dataset.tab;
    if (tab === "alerts" || tab === "laws") {
      this.tab = tab;
      this.render();
      if (tab === "laws") void this.renderLaws();
      return;
    }
    const idx = t.closest<HTMLElement>("[data-alert]")?.dataset.alert;
    if (idx !== undefined) {
      const all = sortAlerts(this.allAlerts());
      const shown = this.selectedPath ? all.filter((a) => a.pathId === this.selectedPath) : all;
      const v = shown[Number(idx)];
      if (!v) return;
      if (v.landing === undefined) void this.globe.focusHop(v.pathId, v.hop);
      else void this.globe.flyToPoint(v.alert.territory.lat, v.alert.territory.lon, 1.5);
    }
  }

  /** Called by the app when a hop is selected: filters the panel to that path. */
  onSelect(sel: HopSelection | null): void {
    const next = sel?.pathId ?? null;
    if (next === this.selectedPath) return;
    this.selectedPath = next;
    if (!this.enabled) return;
    this.render();
    if (this.tab === "laws") void this.renderLaws();
  }

  /** Inspector details for a hop. */
  hopDetails(pathId: string, hop: number): { verdict: string; labels: string[]; point?: ResolvedPoint; alerts: AlertView[] } | null {
    const s = this.state.get(pathId);
    if (!s) return null;
    const path = this.globe.getPath(pathId);
    return { verdict: s.verdict, labels: path ? this.labelsFor(path) : [], point: s.hopPoints[hop], alerts: s.alerts.filter((a) => a.hop === hop) };
  }
}

function esc(s: unknown): string {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);
}
