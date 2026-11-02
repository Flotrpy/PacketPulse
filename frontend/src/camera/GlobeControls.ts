/**
 * Globe camera controls.
 *
 * The camera orbits the globe centre and is described by the surface point
 * under it (lat, lon) and its distance from the centre (globe radius = 1).
 *  - drag: rotate, scaled by altitude so the surface follows the pointer; inertia on release
 *  - wheel / pinch: exponential zoom with damping
 *  - flyTo(lat, lon, distance): eased great-circle flight that pulls back
 *    proportionally to the angle travelled, then dives in ("hop" camera)
 *  - near the surface the view tilts towards the horizon for depth
 * Any user input cancels a running flight.
 */
import * as THREE from "three";
import { angleBetween, latLonToVec3, slerp, vec3ToLatLon, type Vec3 } from "../core/geo";

export interface ControlsOptions {
  minDistance?: number;
  maxDistance?: number;
  /** larger = snappier damping */
  damping?: number;
  /** tilt (radians) at minDistance, fading to 0 by tiltFadeDistance */
  maxTilt?: number;
  tiltFadeDistance?: number;
}

interface Flight {
  fromDir: Vec3;
  toDir: Vec3;
  fromDist: number;
  toDist: number;
  peakDist: number;
  start: number;
  duration: number;
  resolve: () => void;
}

const easeInOutCubic = (t: number) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2);

export class GlobeControls {
  lat = 20;
  lon = 10;
  distance = 3.2;
  private targetLat = 20;
  private targetLon = 10;
  private targetDistance = 3.2;
  private velLat = 0;
  private velLon = 0;
  private flight: Flight | null = null;
  private pointers = new Map<number, { x: number; y: number }>();
  private lastPinch = 0;
  private dragMoved = 0;
  private lastMoveTime = 0;
  readonly opts: Required<ControlsOptions>;

  constructor(
    private camera: THREE.PerspectiveCamera,
    private dom: HTMLElement,
    opts: ControlsOptions = {},
  ) {
    this.opts = {
      minDistance: opts.minDistance ?? 1.12,
      maxDistance: opts.maxDistance ?? 9,
      damping: opts.damping ?? 9,
      maxTilt: opts.maxTilt ?? 0.75,
      tiltFadeDistance: opts.tiltFadeDistance ?? 2.0,
    };
    dom.style.touchAction = "none";
    dom.addEventListener("pointerdown", this.onPointerDown);
    dom.addEventListener("pointermove", this.onPointerMove);
    dom.addEventListener("pointerup", this.onPointerUp);
    dom.addEventListener("pointercancel", this.onPointerUp);
    dom.addEventListener("wheel", this.onWheel, { passive: false });
    this.apply();
  }

  /** Total pointer travel (px) of the last gesture: lets callers tell clicks from drags. */
  get lastDragDistance(): number {
    return this.dragMoved;
  }

  get isFlying(): boolean {
    return this.flight !== null;
  }

  setView(lat: number, lon: number, distance = this.distance): void {
    this.cancelFlight();
    this.lat = this.targetLat = clampLat(lat);
    this.lon = this.targetLon = lon;
    this.distance = this.targetDistance = this.clampDist(distance);
    this.apply();
  }

  /**
   * Smoothly fly to look straight down at (lat, lon) from `distance`.
   * Resolves when the flight lands (or is interrupted).
   */
  flyTo(lat: number, lon: number, distance = this.distance, durationMs?: number): Promise<void> {
    this.cancelFlight();
    const fromDir = latLonToVec3(this.lat, this.lon);
    const toDir = latLonToVec3(clampLat(lat), lon);
    const toDist = this.clampDist(distance);
    const angle = angleBetween(fromDir, toDir);
    // Pull back more for longer flights so the user keeps context.
    const peakDist = Math.min(this.opts.maxDistance, Math.max(this.distance, toDist) + angle * 0.9);
    const duration = durationMs ?? 700 + 900 * (angle / Math.PI) + 250 * Math.abs(Math.log(toDist / this.distance));
    return new Promise((resolve) => {
      this.flight = { fromDir, toDir, fromDist: this.distance, toDist, peakDist, start: performance.now(), duration, resolve };
    });
  }

  cancelFlight(): void {
    if (!this.flight) return;
    const f = this.flight;
    this.flight = null;
    this.targetLat = this.lat;
    this.targetLon = this.lon;
    this.targetDistance = this.distance;
    f.resolve();
  }

  /** Advance damping/flight; call once per frame. Returns true if the camera moved. */
  update(dtSeconds: number, now = performance.now()): boolean {
    const before = this.lat + this.lon * 1e-3 + this.distance * 1e-6;
    if (this.flight) {
      const f = this.flight;
      const t = Math.min(1, (now - f.start) / f.duration);
      const e = easeInOutCubic(t);
      const dir = slerp(f.fromDir, f.toDir, e);
      const ll = vec3ToLatLon(dir);
      // Quadratic Bezier in distance: from -> peak -> to.
      const u = 1 - e;
      const dist = u * u * f.fromDist + 2 * u * e * f.peakDist + e * e * f.toDist;
      this.lat = this.targetLat = clampLat(ll.lat);
      this.lon = this.targetLon = ll.lon;
      this.distance = this.targetDistance = dist;
      if (t >= 1) {
        this.flight = null;
        f.resolve();
      }
    } else {
      // Inertia after a drag.
      if (this.pointers.size === 0 && (Math.abs(this.velLat) > 1e-4 || Math.abs(this.velLon) > 1e-4)) {
        this.targetLat = clampLat(this.targetLat + this.velLat * dtSeconds);
        this.targetLon += this.velLon * dtSeconds;
        const decay = Math.exp(-dtSeconds * 3.5);
        this.velLat *= decay;
        this.velLon *= decay;
      }
      const k = 1 - Math.exp(-dtSeconds * this.opts.damping);
      this.lat += (this.targetLat - this.lat) * k;
      this.lon += shortestDelta(this.lon, this.targetLon) * k;
      this.distance *= Math.pow(this.targetDistance / this.distance, k);
    }
    this.apply();
    return Math.abs(before - (this.lat + this.lon * 1e-3 + this.distance * 1e-6)) > 1e-9;
  }

  /**
   * Position and orient the camera from (lat, lon, distance). Near the
   * ground the camera orbits south of the focus point and looks back at it,
   * giving an oblique horizon view while the focused point stays centred.
   */
  private apply(): void {
    const p = latLonToVec3(this.lat, this.lon);
    const alt = this.distance - 1;
    const tiltK = THREE.MathUtils.clamp((this.opts.tiltFadeDistance - this.distance) / (this.opts.tiltFadeDistance - this.opts.minDistance), 0, 1);
    const tilt = this.opts.maxTilt * tiltK * tiltK;

    const surface = new THREE.Vector3(p[0], p[1], p[2]);
    // Local north tangent at the surface point (lat is clamped away from the poles).
    const north = new THREE.Vector3(0, 1, 0).addScaledVector(surface, -surface.y).normalize();
    const offset = surface.clone().multiplyScalar(Math.cos(tilt)).addScaledVector(north, -Math.sin(tilt)).multiplyScalar(alt);
    this.camera.position.copy(surface).add(offset);
    this.camera.up.copy(tilt > 1e-3 ? north : new THREE.Vector3(0, 1, 0));
    this.camera.lookAt(tilt > 1e-3 ? surface : new THREE.Vector3(0, 0, 0));
    this.camera.near = Math.max(0.0005, alt * 0.25);
    this.camera.far = 200;
    this.camera.updateProjectionMatrix();
  }

  private clampDist(d: number): number {
    return THREE.MathUtils.clamp(d, this.opts.minDistance, this.opts.maxDistance);
  }

  // ---------- input ----------

  private onPointerDown = (e: PointerEvent): void => {
    this.dom.setPointerCapture(e.pointerId);
    this.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (this.pointers.size === 1) this.dragMoved = 0;
    if (this.pointers.size === 2) this.lastPinch = this.pinchDistance();
    this.velLat = this.velLon = 0;
    this.cancelFlight();
  };

  private onPointerMove = (e: PointerEvent): void => {
    const prev = this.pointers.get(e.pointerId);
    if (!prev) return;
    const dx = e.clientX - prev.x;
    const dy = e.clientY - prev.y;
    this.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    this.dragMoved += Math.hypot(dx, dy);

    if (this.pointers.size === 2) {
      const d = this.pinchDistance();
      if (this.lastPinch > 0) this.zoomBy(this.lastPinch / d);
      this.lastPinch = d;
      return;
    }
    // Degrees per pixel so the ground under the cursor roughly follows it.
    const h = this.dom.clientHeight || 1;
    const fov = THREE.MathUtils.degToRad(this.camera.fov);
    const degPerPx = (THREE.MathUtils.radToDeg(2 * Math.tan(fov / 2) * (this.distance - 1)) / h) * 1.0;
    const dLat = dy * degPerPx;
    const dLon = (-dx * degPerPx) / Math.max(0.2, Math.cos(THREE.MathUtils.degToRad(this.lat)));
    this.targetLat = clampLat(this.targetLat + dLat);
    this.targetLon += dLon;
    const now = performance.now();
    const dt = Math.max(1, now - this.lastMoveTime) / 1000;
    this.lastMoveTime = now;
    this.velLat = 0.8 * this.velLat + 0.2 * (dLat / dt);
    this.velLon = 0.8 * this.velLon + 0.2 * (dLon / dt);
  };

  private onPointerUp = (e: PointerEvent): void => {
    this.pointers.delete(e.pointerId);
    if (this.pointers.size < 2) this.lastPinch = 0;
    if (performance.now() - this.lastMoveTime > 80) this.velLat = this.velLon = 0; // released while still
  };

  private onWheel = (e: WheelEvent): void => {
    e.preventDefault();
    this.cancelFlight();
    const delta = e.deltaMode === 1 ? e.deltaY * 33 : e.deltaY;
    this.zoomBy(Math.exp(delta * 0.0015));
  };

  /** factor > 1 zooms out. Zoom acts on altitude, not distance, so it slows near the ground. */
  zoomBy(factor: number): void {
    const alt = Math.max(1e-3, this.targetDistance - 1) * factor;
    this.targetDistance = this.clampDist(1 + alt);
  }

  private pinchDistance(): number {
    const [a, b] = [...this.pointers.values()];
    return Math.hypot(a.x - b.x, a.y - b.y) || 1;
  }

  dispose(): void {
    this.dom.removeEventListener("pointerdown", this.onPointerDown);
    this.dom.removeEventListener("pointermove", this.onPointerMove);
    this.dom.removeEventListener("pointerup", this.onPointerUp);
    this.dom.removeEventListener("pointercancel", this.onPointerUp);
    this.dom.removeEventListener("wheel", this.onWheel);
    this.cancelFlight();
  }
}

function clampLat(lat: number): number {
  return THREE.MathUtils.clamp(lat, -85, 85);
}

function shortestDelta(from: number, to: number): number {
  let d = (to - from) % 360;
  if (d > 180) d -= 360;
  if (d < -180) d += 360;
  return d;
}
