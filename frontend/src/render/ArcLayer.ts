/**
 * GPU-instanced arc layer: one draw call for every packet segment.
 *
 * All per-segment state lives in ONE interleaved Float32Array. Writing a
 * segment touches only its slice, and the dirty slot range is uploaded with
 * addUpdateRange, so a live latency update for one path costs a few hundred
 * bytes of bufferSubData, not a geometry rebuild. Slots are recycled through
 * a free list; the buffer doubles when full.
 */
import * as THREE from "three";
import type { Vec3 } from "../core/geo";
import { arcFragmentShader, arcVertexShader } from "./arcShader";

// Interleaved layout (floats).
const O_P0 = 0;
const O_P1 = 3;
const O_P2 = 6;
const O_P3 = 9;
const O_SEV = 12;
const O_SEV_PREV = 14;
const O_FLASH = 16;
const O_TIMING = 18; // birth, sevChange, segIndex, segCount
const O_STYLE = 22; // phase, highlight, visible, medium
export const STRIDE = 26;

export interface ArcSegment {
  p0: Vec3;
  p1: Vec3;
  p2: Vec3;
  p3: Vec3;
  sevStart: number;
  sevEnd: number;
  flashStart: number;
  flashEnd: number;
  segIndex: number;
  segCount: number;
  birth: number;
  phase: number;
  medium: number;
}

export interface ArcLayerOptions {
  capacity?: number;
  /** curve subdivisions per segment */
  resolution?: number;
  widthPx?: number;
  cycleSeconds?: number;
  drawInSeconds?: number;
  transitionSeconds?: number;
}

export class ArcLayer {
  readonly mesh: THREE.Mesh<THREE.InstancedBufferGeometry, THREE.ShaderMaterial>;
  readonly material: THREE.ShaderMaterial;
  private geometry: THREE.InstancedBufferGeometry;
  private data!: Float32Array;
  private buffer!: THREE.InstancedInterleavedBuffer;
  private capacity: number;
  private highWater = 0;
  private freeSlots: number[] = [];
  private live = 0;
  private dirtyMin = Infinity;
  private dirtyMax = -1;

  constructor(opts: ArcLayerOptions = {}) {
    this.capacity = Math.max(16, opts.capacity ?? 8192);
    const res = opts.resolution ?? 48;

    // Base strip: (res + 1) columns x 2 sides.
    this.geometry = new THREE.InstancedBufferGeometry();
    const pos = new Float32Array((res + 1) * 2 * 3);
    const idx: number[] = [];
    for (let i = 0; i <= res; i++) {
      const t = i / res;
      pos.set([t, -1, 0, t, 1, 0], i * 6);
      if (i < res) {
        // Counter-clockwise in screen space: the shader offsets side +1 to the
        // left of the curve direction, so (start-right, next-right, start-left)
        // faces the camera. The other winding gets back-face culled.
        const a = i * 2;
        idx.push(a, a + 2, a + 1, a + 1, a + 2, a + 3);
      }
    }
    this.geometry.setAttribute("position", new THREE.BufferAttribute(pos, 3));
    this.geometry.setIndex(idx);
    this.allocateBuffer(this.capacity);

    this.material = new THREE.ShaderMaterial({
      vertexShader: arcVertexShader,
      fragmentShader: arcFragmentShader,
      uniforms: {
        uTime: { value: 0 },
        uWidth: { value: opts.widthPx ?? 3.0 },
        uDrawIn: { value: opts.drawInSeconds ?? 1.4 },
        uTransition: { value: opts.transitionSeconds ?? 0.8 },
        uCycle: { value: opts.cycleSeconds ?? 3.2 },
        uResolution: { value: new THREE.Vector2(1, 1) },
        uFocusActive: { value: 0 },
      },
      transparent: true,
      depthWrite: false,
      depthTest: true,
      blending: THREE.AdditiveBlending,
    });
    this.mesh = new THREE.Mesh(this.geometry, this.material);
    this.mesh.frustumCulled = false; // positions are computed on the GPU
    this.mesh.renderOrder = 2;
  }

  private allocateBuffer(capacity: number): void {
    const next = new Float32Array(capacity * STRIDE);
    if (this.data) next.set(this.data);
    this.data = next;
    this.buffer = new THREE.InstancedInterleavedBuffer(this.data, STRIDE, 1);
    this.buffer.setUsage(THREE.DynamicDrawUsage);
    const attr = (name: string, size: number, offset: number) =>
      this.geometry.setAttribute(name, new THREE.InterleavedBufferAttribute(this.buffer, size, offset));
    attr("aP0", 3, O_P0);
    attr("aP1", 3, O_P1);
    attr("aP2", 3, O_P2);
    attr("aP3", 3, O_P3);
    attr("aSev", 2, O_SEV);
    attr("aSevPrev", 2, O_SEV_PREV);
    attr("aFlash", 2, O_FLASH);
    attr("aTiming", 4, O_TIMING);
    attr("aStyle", 4, O_STYLE);
    this.capacity = capacity;
    this.dirtyMin = Infinity;
    this.dirtyMax = -1;
    this.buffer.needsUpdate = true; // full upload of the new buffer
  }

  get count(): number {
    return this.live;
  }

  get instanceCount(): number {
    return this.highWater;
  }

  alloc(): number {
    let slot = this.freeSlots.pop();
    if (slot === undefined) {
      if (this.highWater >= this.capacity) this.allocateBuffer(this.capacity * 2);
      slot = this.highWater++;
      this.geometry.instanceCount = this.highWater;
    }
    this.live++;
    return slot;
  }

  release(slot: number): void {
    this.data[slot * STRIDE + O_STYLE + 2] = 0; // invisible
    this.markDirty(slot);
    this.freeSlots.push(slot);
    this.live--;
  }

  write(slot: number, s: ArcSegment): void {
    const o = slot * STRIDE;
    const d = this.data;
    d.set(s.p0, o + O_P0);
    d.set(s.p1, o + O_P1);
    d.set(s.p2, o + O_P2);
    d.set(s.p3, o + O_P3);
    d[o + O_SEV] = d[o + O_SEV_PREV] = s.sevStart;
    d[o + O_SEV + 1] = d[o + O_SEV_PREV + 1] = s.sevEnd;
    d[o + O_FLASH] = s.flashStart;
    d[o + O_FLASH + 1] = s.flashEnd;
    d[o + O_TIMING] = s.birth;
    d[o + O_TIMING + 1] = -1e6;
    d[o + O_TIMING + 2] = s.segIndex;
    d[o + O_TIMING + 3] = s.segCount;
    d[o + O_STYLE] = s.phase;
    d[o + O_STYLE + 1] = 0;
    d[o + O_STYLE + 2] = 1;
    d[o + O_STYLE + 3] = s.medium;
    this.markDirty(slot);
  }

  /**
   * Retarget a segment's colours. The colour currently on screen (which may
   * be mid-transition) becomes the start of a new transition, so rapid
   * updates never jump.
   */
  setSeverity(slot: number, sevStart: number, sevEnd: number, flashStart: number, flashEnd: number, now: number): void {
    const o = slot * STRIDE;
    const d = this.data;
    const transition = this.material.uniforms.uTransition.value as number;
    let k = Math.min(1, Math.max(0, (now - d[o + O_TIMING + 1]) / transition));
    k = k * k * (3 - 2 * k);
    const curA = d[o + O_SEV_PREV] + (d[o + O_SEV] - d[o + O_SEV_PREV]) * k;
    const curB = d[o + O_SEV_PREV + 1] + (d[o + O_SEV + 1] - d[o + O_SEV_PREV + 1]) * k;
    d[o + O_SEV_PREV] = curA;
    d[o + O_SEV_PREV + 1] = curB;
    d[o + O_SEV] = sevStart;
    d[o + O_SEV + 1] = sevEnd;
    d[o + O_FLASH] = flashStart;
    d[o + O_FLASH + 1] = flashEnd;
    d[o + O_TIMING + 1] = now;
    this.markDirty(slot);
  }

  setHighlight(slot: number, on: boolean): void {
    this.data[slot * STRIDE + O_STYLE + 1] = on ? 1 : 0;
    this.markDirty(slot);
  }

  setFocusActive(active: boolean): void {
    this.material.uniforms.uFocusActive.value = active ? 1 : 0;
  }

  private markDirty(slot: number): void {
    if (slot < this.dirtyMin) this.dirtyMin = slot;
    if (slot > this.dirtyMax) this.dirtyMax = slot;
  }

  /** Upload changed slots (one contiguous range) before rendering. */
  flush(): void {
    if (this.dirtyMax < 0) return;
    this.buffer.clearUpdateRanges();
    this.buffer.addUpdateRange(this.dirtyMin * STRIDE, (this.dirtyMax - this.dirtyMin + 1) * STRIDE);
    this.buffer.needsUpdate = true;
    this.dirtyMin = Infinity;
    this.dirtyMax = -1;
  }

  update(time: number, widthPx: number, heightPx: number): void {
    const u = this.material.uniforms;
    u.uTime.value = time;
    (u.uResolution.value as THREE.Vector2).set(widthPx, heightPx);
    this.flush();
  }

  /** Bytes queued for the next upload (for stats/tests). */
  pendingUploadFloats(): number {
    return this.dirtyMax < 0 ? 0 : (this.dirtyMax - this.dirtyMin + 1) * STRIDE;
  }

  dispose(): void {
    this.geometry.dispose();
    this.material.dispose();
  }
}
