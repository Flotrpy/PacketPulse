/**
 * Hop markers: every hop of every path is one vertex of a single THREE.Points
 * draw call. Per-hop state is written into preallocated buffer attributes;
 * only the changed range is uploaded.
 */
import * as THREE from "three";
import { GLSL_HEALTH } from "../core/health";
import type { Vec3 } from "../core/geo";

const vertexShader = /* glsl */ `
attribute float aSev;
attribute float aFlash;
attribute vec2 aState; // x = visible, y = 0 normal | 1 path highlighted | 2 selected hop
uniform float uSize;
uniform float uPixelRatio;
uniform float uFocusActive;
varying float vSev;
varying float vFlash;
varying float vSelected;
varying float vDim;
void main() {
  if (aState.x < 0.5) { gl_Position = vec4(2.0, 2.0, 2.0, 1.0); gl_PointSize = 0.0; return; }
  vec4 mv = modelViewMatrix * vec4(position, 1.0);
  gl_Position = projectionMatrix * mv;
  float sel = step(1.5, aState.y);
  float hi = step(0.5, aState.y);
  // Shrink with distance but keep a readable minimum.
  float size = uSize * (1.0 + 0.6 * hi + 1.4 * sel) * clamp(3.2 / -mv.z, 0.55, 2.2);
  gl_PointSize = size * uPixelRatio;
  vSev = aSev;
  vFlash = aFlash;
  vSelected = sel;
  vDim = mix(1.0, mix(0.25, 1.0, hi), uFocusActive);
}
`;

const fragmentShader = /* glsl */ `
uniform float uTime;
varying float vSev;
varying float vFlash;
varying float vSelected;
varying float vDim;
${GLSL_HEALTH}
void main() {
  vec2 p = gl_PointCoord * 2.0 - 1.0;
  float r = length(p);
  if (r > 1.0) discard;
  vec3 col = severityColor(vSev);
  float flash = vFlash * (0.5 + 0.5 * sin(uTime * 13.0));
  col = mix(col, vec3(1.0, 0.06, 0.06), flash * 0.85);
  float dotCore = 1.0 - smoothstep(0.35, 0.5, r);
  float halo = (1.0 - r) * 0.6;
  // Selected hop: expanding ring.
  float ringR = 0.6 + 0.35 * fract(uTime * 0.8);
  float ring = vSelected * (1.0 - smoothstep(0.0, 0.08, abs(r - ringR))) * (1.0 - fract(uTime * 0.8));
  float rim = smoothstep(0.32, 0.42, r) * (1.0 - smoothstep(0.42, 0.5, r));
  vec3 rgb = mix(col, vec3(0.02, 0.04, 0.08), rim * 0.8) + vec3(1.0) * ring;
  float a = clamp(dotCore + halo * 0.5 + ring, 0.0, 1.0) * vDim;
  gl_FragColor = vec4(rgb, a);
}
`;

export const HOP_RADIUS = 1.003;

export class HopLayer {
  readonly points: THREE.Points<THREE.BufferGeometry, THREE.ShaderMaterial>;
  private geometry = new THREE.BufferGeometry();
  private material: THREE.ShaderMaterial;
  private capacity: number;
  private highWater = 0;
  private freeSlots: number[] = [];
  private pos!: THREE.BufferAttribute;
  private sev!: THREE.BufferAttribute;
  private flash!: THREE.BufferAttribute;
  private state!: THREE.BufferAttribute;
  private dirtyMin = Infinity;
  private dirtyMax = -1;
  /** CPU copy of positions for picking. */
  positions!: Float32Array;

  constructor(capacity = 8192, sizePx = 7) {
    this.capacity = capacity;
    this.allocate(capacity);
    this.material = new THREE.ShaderMaterial({
      vertexShader,
      fragmentShader,
      uniforms: { uSize: { value: sizePx }, uPixelRatio: { value: 1 }, uTime: { value: 0 }, uFocusActive: { value: 0 } },
      transparent: true,
      depthWrite: false,
      // Normal blending: stacked markers at busy hubs keep their colour.
      blending: THREE.NormalBlending,
    });
    this.points = new THREE.Points(this.geometry, this.material);
    this.points.frustumCulled = false;
    this.points.renderOrder = 3;
  }

  private allocate(capacity: number): void {
    const grow = <T extends THREE.BufferAttribute | undefined>(old: T, size: number) => {
      const arr = new Float32Array(capacity * size);
      if (old) arr.set(old.array as Float32Array);
      const a = new THREE.BufferAttribute(arr, size);
      a.setUsage(THREE.DynamicDrawUsage);
      return a;
    };
    this.pos = grow(this.pos, 3);
    this.sev = grow(this.sev, 1);
    this.flash = grow(this.flash, 1);
    this.state = grow(this.state, 2);
    this.positions = this.pos.array as Float32Array;
    this.geometry.setAttribute("position", this.pos);
    this.geometry.setAttribute("aSev", this.sev);
    this.geometry.setAttribute("aFlash", this.flash);
    this.geometry.setAttribute("aState", this.state);
    this.capacity = capacity;
    this.dirtyMin = Infinity;
    this.dirtyMax = -1;
  }

  alloc(): number {
    let slot = this.freeSlots.pop();
    if (slot === undefined) {
      if (this.highWater >= this.capacity) this.allocate(this.capacity * 2);
      slot = this.highWater++;
      this.geometry.setDrawRange(0, this.highWater);
    }
    return slot;
  }

  release(slot: number): void {
    (this.state.array as Float32Array)[slot * 2] = 0;
    this.mark(slot);
    this.freeSlots.push(slot);
  }

  write(slot: number, p: Vec3, severity: number, flash: boolean): void {
    (this.pos.array as Float32Array).set([p[0] * HOP_RADIUS, p[1] * HOP_RADIUS, p[2] * HOP_RADIUS], slot * 3);
    (this.sev.array as Float32Array)[slot] = severity;
    (this.flash.array as Float32Array)[slot] = flash ? 1 : 0;
    const st = this.state.array as Float32Array;
    st[slot * 2] = 1;
    this.mark(slot);
  }

  setSeverity(slot: number, severity: number, flash: boolean): void {
    (this.sev.array as Float32Array)[slot] = severity;
    (this.flash.array as Float32Array)[slot] = flash ? 1 : 0;
    this.mark(slot);
  }

  /** 0 normal, 1 on highlighted path, 2 selected hop */
  setState(slot: number, level: 0 | 1 | 2): void {
    (this.state.array as Float32Array)[slot * 2 + 1] = level;
    this.mark(slot);
  }

  isVisible(slot: number): boolean {
    return (this.state.array as Float32Array)[slot * 2] > 0.5;
  }

  get slotCount(): number {
    return this.highWater;
  }

  setFocusActive(active: boolean): void {
    this.material.uniforms.uFocusActive.value = active ? 1 : 0;
  }

  private mark(slot: number): void {
    if (slot < this.dirtyMin) this.dirtyMin = slot;
    if (slot > this.dirtyMax) this.dirtyMax = slot;
  }

  update(time: number, pixelRatio: number): void {
    this.material.uniforms.uTime.value = time;
    this.material.uniforms.uPixelRatio.value = pixelRatio;
    if (this.dirtyMax < 0) return;
    const n = this.dirtyMax - this.dirtyMin + 1;
    for (const [attr, size] of [[this.pos, 3], [this.sev, 1], [this.flash, 1], [this.state, 2]] as const) {
      attr.clearUpdateRanges();
      attr.addUpdateRange(this.dirtyMin * size, n * size);
      attr.needsUpdate = true;
    }
    this.dirtyMin = Infinity;
    this.dirtyMax = -1;
  }

  dispose(): void {
    this.geometry.dispose();
    this.material.dispose();
  }
}
