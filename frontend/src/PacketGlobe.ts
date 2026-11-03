/**
 * PacketGlobe: the embeddable visualisation component.
 *
 *   const globe = new PacketGlobe({ container });
 *   globe.setPath({ id, hops: [{ lat, lon, rttMs, jitterMs, lossPct }, ...] });
 *   globe.onSelect((sel) => ...);
 *
 * Paths are diffed: re-sending a path with the same hop locations only
 * retargets colours (smooth GPU transition) and uploads those few slots;
 * changed geometry rewrites the path's slots in place.
 * Draw calls: Earth (sphere, atmosphere, borders, stars) + 1 for ALL arcs + 1 for ALL hops.
 */
import * as THREE from "three";
import { GlobeControls, type ControlsOptions } from "./camera/GlobeControls";
import { arcControls } from "./core/curve";
import { angleBetween, EARTH_RADIUS_KM, latLonToVec3, type Vec3 } from "./core/geo";
import { segmentHealth, type SegmentHealth } from "./core/health";
import type { HopInput, HopSelection, PathInput } from "./core/types";
import { ArcLayer } from "./render/ArcLayer";
import { MEDIUM_CODE } from "./render/arcShader";
import { Earth } from "./render/Earth";
import { HopLayer } from "./render/HopLayer";
import { AlertLayer, type AlertMarker } from "./render/AlertLayer";
import type { GeoJSONFeatureCollection } from "./render/Earth";

export interface PacketGlobeOptions {
  container: HTMLElement;
  /** URL of land.json (Natural Earth basemap); null to skip */
  landUrl?: string | null;
  /** Base URL of the realistic Earth textures (null = stylised globe only). */
  texturesUrl?: string | null;
  maxPixelRatio?: number;
  arcCapacity?: number;
  hopCapacity?: number;
  arcWidthPx?: number;
  hopSizePx?: number;
  /** seconds for a packet pulse to traverse a path */
  pulseCycleSeconds?: number;
  controls?: ControlsOptions;
  /** max px between click and hop marker for a hit */
  pickRadiusPx?: number;
}

interface PathRecord {
  input: PathInput;
  dirs: Vec3[];
  health: SegmentHealth[]; // health[i] = link arriving at hop i (health[0] = source, healthy)
  arcSlots: number[];
  hopSlots: number[];
  birth: number;
  phase: number;
  /** Business Mode status per hop (core/compliance.ts codes); cleared on reroute. */
  compliance?: PathCompliance;
}

/** Compliance codes for one path: per hop, plus per segment end (a segment can
 *  touch a non-permitted landing station between two compliant hops). */
export interface PathCompliance {
  hops: number[];
  segmentEnds?: number[];
}

/** What changed when paths were added, rerouted or removed. */
export interface PathChange {
  rerouted: string[]; // new paths or new geometry: compliance must be re-evaluated
  removed: string[];
}

export interface GlobeStats {
  paths: number;
  segments: number;
  hops: number;
  drawCalls: number;
  triangles: number;
  fps: number;
}

export class PacketGlobe {
  readonly renderer: THREE.WebGLRenderer;
  readonly scene = new THREE.Scene();
  readonly camera: THREE.PerspectiveCamera;
  readonly controls: GlobeControls;
  readonly earth = new Earth();
  readonly arcs: ArcLayer;
  readonly hops: HopLayer;
  readonly alerts = new AlertLayer();
  private business = false;
  private changeListeners = new Set<(c: PathChange) => void>();
  private pendingChange: PathChange = { rerouted: [], removed: [] };
  private changeTimer = 0;

  private paths = new Map<string, PathRecord>();
  private hopOwner: { pathId: string; hopIndex: number }[] = [];
  private selection: { pathId: string; hopIndex: number } | null = null;
  private listeners = new Set<(sel: HopSelection | null) => void>();
  private timer = new THREE.Timer();
  private frame = 0;
  private resizeObs: ResizeObserver;
  private fps = 0;
  private fpsFrames = 0;
  private fpsT0 = performance.now();
  private disposed = false;
  private downAt = { x: 0, y: 0 };
  private opts: PacketGlobeOptions;

  constructor(opts: PacketGlobeOptions) {
    this.opts = opts;
    const { container } = opts;
    this.renderer = new THREE.WebGLRenderer({ antialias: true, alpha: false, powerPreference: "high-performance" });
    this.renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, opts.maxPixelRatio ?? 2));
    this.renderer.setClearColor(0x02040a, 1);
    container.appendChild(this.renderer.domElement);
    this.renderer.domElement.style.display = "block";

    this.camera = new THREE.PerspectiveCamera(45, 1, 0.01, 200);
    this.controls = new GlobeControls(this.camera, this.renderer.domElement, opts.controls);

    this.arcs = new ArcLayer({ capacity: opts.arcCapacity, widthPx: opts.arcWidthPx, cycleSeconds: opts.pulseCycleSeconds });
    this.hops = new HopLayer(opts.hopCapacity, opts.hopSizePx);
    this.scene.add(this.earth.group, this.arcs.mesh, this.hops.points, this.alerts.points);

    if (opts.landUrl !== null) {
      this.earth.loadLand(opts.landUrl ?? "data/land.json").catch((err) => console.warn("PacketGlobe: basemap not loaded", err));
      if (opts.texturesUrl !== null) this.earth.loadTextures(opts.texturesUrl ?? "textures/", this.renderer).catch((err) => console.warn("PacketGlobe: realistic Earth textures not loaded", err));
    }

    this.resizeObs = new ResizeObserver(() => this.resize());
    this.resizeObs.observe(container);
    this.resize();

    const el = this.renderer.domElement;
    el.addEventListener("pointerdown", this.onDown);
    el.addEventListener("pointerup", this.onUp);
    this.timer.connect(document); // pauses time while the tab is hidden
    this.frame = requestAnimationFrame(this.loop);
  }

  // ---------------- public API ----------------

  /** Add or update a path. */
  setPath(input: PathInput): void {
    if (input.hops.length < 2) {
      this.removePath(input.id);
      return;
    }
    const now = this.time();
    const dirs = input.hops.map((h) => latLonToVec3(h.lat, h.lon));
    const health = computeHealth(input.hops, dirs);
    const existing = this.paths.get(input.id);

    if (existing && sameGeometry(existing.dirs, dirs)) {
      // Live metrics update: retarget colours only.
      existing.input = input;
      existing.health = health;
      for (let k = 0; k < existing.arcSlots.length; k++) {
        const [s0, s1, f0, f1] = segColors(health, k);
        this.arcs.setSeverity(existing.arcSlots[k], s0, s1, f0, f1, now);
      }
      existing.hopSlots.forEach((slot, i) => this.hops.setSeverity(slot, health[i].severity, health[i].flash));
      this.emitIfSelected(input.id);
      return;
    }

    const rec: PathRecord = existing ?? { input, dirs, health, arcSlots: [], hopSlots: [], birth: now, phase: hashPhase(input.id) };
    rec.input = input;
    rec.dirs = dirs;
    rec.health = health;
    if (existing) rec.birth = now; // redraw-in on reroute
    rec.compliance = undefined;
    this.resizeSlots(rec, dirs.length);

    const segCount = dirs.length - 1;
    for (let k = 0; k < segCount; k++) {
      const c = arcControls(dirs[k], dirs[k + 1]);
      const [s0, s1, f0, f1] = segColors(health, k);
      this.arcs.write(rec.arcSlots[k], {
        ...c,
        sevStart: s0,
        sevEnd: s1,
        flashStart: f0,
        flashEnd: f1,
        segIndex: k,
        segCount,
        birth: rec.birth,
        phase: rec.phase,
        medium: MEDIUM_CODE[input.hops[k + 1].medium ?? "unknown"] ?? 0,
      });
    }
    rec.hopSlots.forEach((slot, i) => {
      this.hops.write(slot, dirs[i], health[i].severity, health[i].flash);
      this.hopOwner[slot] = { pathId: input.id, hopIndex: i };
    });
    this.paths.set(input.id, rec);
    this.queueChange(input.id, false);
    if (this.selection?.pathId === input.id) {
      this.selection.hopIndex = Math.min(this.selection.hopIndex, dirs.length - 1);
      this.applyHighlight(input.id, true);
      this.emitIfSelected(input.id);
    }
  }

  removePath(id: string): void {
    const rec = this.paths.get(id);
    if (!rec) return;
    rec.arcSlots.forEach((s) => this.arcs.release(s));
    rec.hopSlots.forEach((s) => this.hops.release(s));
    this.paths.delete(id);
    this.queueChange(id, true);
    if (this.selection?.pathId === id) this.clearSelection();
  }

  // ---------------- Business Mode ----------------

  /** Colour arcs and hops by compliance instead of latency, and show the jurisdiction overlay and alert markers. */
  setBusinessMode(on: boolean): void {
    this.business = on;
    this.arcs.setBusinessMode(on);
    this.hops.setBusinessMode(on);
    this.earth.setBusinessMode(on);
    this.alerts.points.visible = on;
  }

  get businessMode(): boolean {
    return this.business;
  }

  /** Apply compliance codes to a path's hops and segments (see PathCompliance). */
  setPathCompliance(pathId: string, c: PathCompliance): void {
    const rec = this.paths.get(pathId);
    if (!rec || c.hops.length !== rec.hopSlots.length) return;
    rec.compliance = c;
    rec.hopSlots.forEach((slot, i) => this.hops.setCompliance(slot, c.hops[i]));
    rec.arcSlots.forEach((slot, k) => this.arcs.setCompliance(slot, c.hops[k], Math.max(c.hops[k + 1], c.segmentEnds?.[k] ?? 0)));
    this.emitIfSelected(pathId);
  }

  getPathCompliance(pathId: string): PathCompliance | undefined {
    return this.paths.get(pathId)?.compliance;
  }

  clearCompliance(): void {
    for (const [, rec] of this.paths) {
      rec.compliance = undefined;
      rec.hopSlots.forEach((s) => this.hops.setCompliance(s, 0));
      rec.arcSlots.forEach((s) => this.arcs.setCompliance(s, 0, 0));
    }
    this.alerts.setMarkers([]);
  }

  setAlertMarkers(markers: AlertMarker[]): void {
    this.alerts.setMarkers(markers);
  }

  setRegionOverlay(permitted: GeoJSONFeatureCollection[], prohibited: GeoJSONFeatureCollection[]): void {
    this.earth.setRegionOverlay(permitted, prohibited);
  }

  /** Fly the camera to any coordinate (e.g. an alert's territory entry point). */
  flyToPoint(lat: number, lon: number, distance = 1.6): Promise<void> {
    return this.controls.flyTo(lat, lon, distance);
  }

  /**
   * Listen for path changes. Notifications are batched (~150 ms) so a
   * simulator tick that reroutes dozens of paths produces one callback.
   * Latency-only updates are not reported: they cannot change compliance.
   */
  onPathsChanged(cb: (c: PathChange) => void): () => void {
    this.changeListeners.add(cb);
    return () => this.changeListeners.delete(cb);
  }

  private queueChange(id: string, removed: boolean): void {
    if (this.changeListeners.size === 0) return;
    (removed ? this.pendingChange.removed : this.pendingChange.rerouted).push(id);
    if (this.changeTimer) return;
    this.changeTimer = window.setTimeout(() => {
      const c = this.pendingChange;
      this.pendingChange = { rerouted: [], removed: [] };
      this.changeTimer = 0;
      const removedSet = new Set(c.removed);
      c.rerouted = [...new Set(c.rerouted)].filter((id) => this.paths.has(id));
      c.removed = [...removedSet].filter((id) => !this.paths.has(id));
      for (const l of this.changeListeners) l(c);
    }, 150);
  }

  clear(): void {
    for (const id of [...this.paths.keys()]) this.removePath(id);
  }

  getPath(id: string): PathInput | undefined {
    return this.paths.get(id)?.input;
  }

  get pathIds(): string[] {
    return [...this.paths.keys()];
  }

  onSelect(cb: (sel: HopSelection | null) => void): () => void {
    this.listeners.add(cb);
    return () => this.listeners.delete(cb);
  }

  /** Select a hop, highlight its path and fly the camera to it. */
  async focusHop(pathId: string, hopIndex: number, distance?: number): Promise<void> {
    const rec = this.paths.get(pathId);
    if (!rec) return;
    const i = Math.max(0, Math.min(hopIndex, rec.dirs.length - 1));
    this.select(pathId, i);
    const hop = rec.input.hops[i];
    // Zoom so the nearest neighbouring hop stays in view: metro hops get a
    // close street-level view, ocean crossings a regional one.
    let span = Infinity;
    for (const j of [i - 1, i + 1]) {
      if (j < 0 || j >= rec.dirs.length) continue;
      const a = angleBetween(rec.dirs[i], rec.dirs[j]);
      if (a > 1e-4) span = Math.min(span, a);
    }
    if (!Number.isFinite(span)) span = 0.2;
    const d = distance ?? THREE.MathUtils.clamp(1.2 + span * 1.8, 1.2, 2.6);
    await this.controls.flyTo(hop.lat, hop.lon, d);
  }

  /** Frame a whole path. */
  async focusPath(pathId: string): Promise<void> {
    const rec = this.paths.get(pathId);
    if (!rec) return;
    const c: Vec3 = [0, 0, 0];
    for (const d of rec.dirs) for (let k = 0; k < 3; k++) c[k] += d[k];
    const len = Math.hypot(...c);
    let maxAngle = 0;
    const centre: Vec3 = len > 1e-6 ? [c[0] / len, c[1] / len, c[2] / len] : rec.dirs[0];
    for (const d of rec.dirs) maxAngle = Math.max(maxAngle, angleBetween(centre, d));
    this.select(pathId, 0, false);
    const lat = (Math.asin(centre[1]) * 180) / Math.PI;
    const lon = (Math.atan2(centre[0], centre[2]) * 180) / Math.PI;
    await this.controls.flyTo(lat, lon, THREE.MathUtils.clamp(1.4 + maxAngle * 2.4, 1.4, 6));
  }

  clearSelection(): void {
    if (this.selection) {
      this.applyHighlight(this.selection.pathId, false);
      this.selection = null;
    }
    this.arcs.setFocusActive(false);
    this.hops.setFocusActive(false);
    for (const l of this.listeners) l(null);
  }

  /** Nearest visible hop within pickRadiusPx of a canvas-relative point. */
  pick(x: number, y: number): { pathId: string; hopIndex: number } | null {
    const el = this.renderer.domElement;
    const w = el.clientWidth;
    const h = el.clientHeight;
    const radius = this.opts.pickRadiusPx ?? 14;
    const cam = this.camera.position;
    const pos = this.hops.positions;
    const v = new THREE.Vector3();
    let best = radius * radius;
    let hit: { pathId: string; hopIndex: number } | null = null;
    for (let s = 0; s < this.hops.slotCount; s++) {
      if (!this.hops.isVisible(s)) continue;
      const px = pos[s * 3], py = pos[s * 3 + 1], pz = pos[s * 3 + 2];
      // Front hemisphere only: p.(cam - p) > 0.
      if (px * (cam.x - px) + py * (cam.y - py) + pz * (cam.z - pz) <= 0) continue;
      v.set(px, py, pz).project(this.camera);
      if (v.z > 1) continue;
      const sx = ((v.x + 1) / 2) * w;
      const sy = ((1 - v.y) / 2) * h;
      const d2 = (sx - x) ** 2 + (sy - y) ** 2;
      if (d2 < best) {
        best = d2;
        hit = this.hopOwner[s];
      }
    }
    return hit;
  }

  stats(): GlobeStats {
    let hops = 0;
    for (const r of this.paths.values()) hops += r.hopSlots.length;
    return {
      paths: this.paths.size,
      segments: this.arcs.count,
      hops,
      drawCalls: this.renderer.info.render.calls,
      triangles: this.renderer.info.render.triangles,
      fps: this.fps,
    };
  }

  dispose(): void {
    this.disposed = true;
    cancelAnimationFrame(this.frame);
    this.timer.dispose();
    this.resizeObs.disconnect();
    const el = this.renderer.domElement;
    el.removeEventListener("pointerdown", this.onDown);
    el.removeEventListener("pointerup", this.onUp);
    this.controls.dispose();
    this.arcs.dispose();
    this.hops.dispose();
    this.alerts.dispose();
    clearTimeout(this.changeTimer);
    this.earth.dispose();
    this.renderer.dispose();
    el.remove();
  }

  // ---------------- internals ----------------

  private time(): number {
    return this.timer.getElapsed();
  }

  private resizeSlots(rec: PathRecord, hopCount: number): void {
    const segs = hopCount - 1;
    while (rec.arcSlots.length > segs) this.arcs.release(rec.arcSlots.pop()!);
    while (rec.arcSlots.length < segs) rec.arcSlots.push(this.arcs.alloc());
    while (rec.hopSlots.length > hopCount) this.hops.release(rec.hopSlots.pop()!);
    while (rec.hopSlots.length < hopCount) rec.hopSlots.push(this.hops.alloc());
  }

  private select(pathId: string, hopIndex: number, markHop = true): void {
    if (this.selection) this.applyHighlight(this.selection.pathId, false);
    this.selection = { pathId, hopIndex: markHop ? hopIndex : -1 };
    this.applyHighlight(pathId, true);
    this.arcs.setFocusActive(true);
    this.hops.setFocusActive(true);
    this.emitIfSelected(pathId);
  }

  private applyHighlight(pathId: string, on: boolean): void {
    const rec = this.paths.get(pathId);
    if (!rec) return;
    rec.arcSlots.forEach((s) => this.arcs.setHighlight(s, on));
    rec.hopSlots.forEach((s, i) => this.hops.setState(s, on ? (this.selection?.hopIndex === i ? 2 : 1) : 0));
  }

  private emitIfSelected(pathId: string): void {
    if (!this.selection || this.selection.pathId !== pathId) return;
    const rec = this.paths.get(pathId);
    if (!rec) return;
    const i = Math.max(0, this.selection.hopIndex);
    const sel: HopSelection = { pathId, hopIndex: i, hop: rec.input.hops[i], severity: rec.health[i].severity, latencyExcess: rec.health[i].latencyExcess };
    for (const l of this.listeners) l(sel);
  }

  private onDown = (e: PointerEvent): void => {
    this.downAt = { x: e.clientX, y: e.clientY };
  };

  private onUp = (e: PointerEvent): void => {
    // A click, not a drag.
    if (Math.hypot(e.clientX - this.downAt.x, e.clientY - this.downAt.y) > 5) return;
    const r = this.renderer.domElement.getBoundingClientRect();
    const hit = this.pick(e.clientX - r.left, e.clientY - r.top);
    if (hit) void this.focusHop(hit.pathId, hit.hopIndex);
    else if (this.selection) this.clearSelection();
  };

  private resize(): void {
    const c = this.opts.container;
    const w = Math.max(1, c.clientWidth);
    const h = Math.max(1, c.clientHeight);
    this.renderer.setSize(w, h, true);
    this.camera.aspect = w / h;
    this.camera.updateProjectionMatrix();
  }

  private loop = (): void => {
    if (this.disposed) return;
    this.frame = requestAnimationFrame(this.loop);
    this.timer.update();
    if (document.visibilityState === "hidden") return;
    const dt = Math.min(0.1, this.timer.getDelta());
    const t = this.timer.getElapsed();
    this.controls.update(dt);
    const size = this.renderer.getDrawingBufferSize(new THREE.Vector2());
    this.earth.update(t);
    this.arcs.update(t, size.x, size.y);
    this.hops.update(t, this.renderer.getPixelRatio());
    if (this.business) this.alerts.update(t, this.renderer.getPixelRatio());
    this.renderer.render(this.scene, this.camera);

    this.fpsFrames++;
    const now = performance.now();
    if (now - this.fpsT0 >= 500) {
      this.fps = Math.round((this.fpsFrames * 1000) / (now - this.fpsT0));
      this.fpsFrames = 0;
      this.fpsT0 = now;
    }
  };
}

// ---------------- pure helpers (exported for tests) ----------------

export function computeHealth(hops: HopInput[], dirs: Vec3[]): SegmentHealth[] {
  const out: SegmentHealth[] = [{ severity: 0, flash: false, latencyExcess: 0 }];
  for (let i = 1; i < hops.length; i++) {
    const km = angleBetween(dirs[i - 1], dirs[i]) * EARTH_RADIUS_KM;
    out.push(segmentHealth(hops[i - 1], hops[i], km));
  }
  return out;
}

/** Colours for segment k (hop k -> k+1): gradient from hop k's health to hop k+1's; loss flashes towards the lossy hop. */
export function segColors(health: SegmentHealth[], k: number): [number, number, number, number] {
  const a = health[k];
  const b = health[k + 1];
  return [a.severity, b.severity, b.flash ? 0.35 : 0, b.flash ? 1 : 0];
}

function sameGeometry(a: Vec3[], b: Vec3[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (Math.abs(a[i][0] - b[i][0]) > 1e-9 || Math.abs(a[i][1] - b[i][1]) > 1e-9 || Math.abs(a[i][2] - b[i][2]) > 1e-9) return false;
  }
  return true;
}

/** Stable pulse phase per path id so pulses don't march in lock-step. */
export function hashPhase(id: string): number {
  let h = 2166136261;
  for (let i = 0; i < id.length; i++) h = Math.imul(h ^ id.charCodeAt(i), 16777619);
  return ((h >>> 0) % 10000) / 10000;
}
