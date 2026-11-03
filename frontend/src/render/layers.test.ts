import { describe, expect, it } from "vitest";
import { arcControls } from "../core/curve";
import { latLonToVec3 } from "../core/geo";
import { ArcLayer, STRIDE, type ArcSegment } from "./ArcLayer";
import { HopLayer } from "./HopLayer";

const seg = (i: number): ArcSegment => ({
  ...arcControls(latLonToVec3(0, i), latLonToVec3(10, i + 10)),
  sevStart: 0.1,
  sevEnd: 0.2,
  flashStart: 0,
  flashEnd: 0,
  segIndex: 0,
  segCount: 1,
  birth: 0,
  phase: 0.5,
  medium: 5,
});

const buf = (layer: ArcLayer) => layer.mesh.geometry.getAttribute("aP0") as unknown as { data: { array: Float32Array; updateRanges: { start: number; count: number }[] } };

describe("ArcLayer", () => {
  it("draws every segment as an instance of one mesh", () => {
    const layer = new ArcLayer({ capacity: 64 });
    for (let i = 0; i < 40; i++) layer.write(layer.alloc(), seg(i));
    expect(layer.count).toBe(40);
    expect(layer.mesh.geometry.instanceCount).toBe(40);
    // All per-instance attributes share one interleaved buffer.
    const g = layer.mesh.geometry;
    const data = buf(layer).data;
    for (const name of ["aP0", "aP1", "aP2", "aP3", "aSev", "aSevPrev", "aFlash", "aTiming", "aStyle"]) {
      expect((g.getAttribute(name) as unknown as { data: unknown }).data).toBe(data);
    }
  });

  it("recycles freed slots and hides them", () => {
    const layer = new ArcLayer({ capacity: 16 });
    const slots = [layer.alloc(), layer.alloc(), layer.alloc()];
    slots.forEach((s, i) => layer.write(s, seg(i)));
    layer.release(slots[1]);
    expect(buf(layer).data.array[slots[1] * STRIDE + 24]).toBe(0); // visible flag
    expect(layer.alloc()).toBe(slots[1]);
    expect(layer.instanceCount).toBe(3);
  });

  it("uploads only the dirty slot range", () => {
    const layer = new ArcLayer({ capacity: 1024 });
    const slots = Array.from({ length: 500 }, () => layer.alloc());
    slots.forEach((s, i) => layer.write(s, seg(i)));
    layer.flush();
    layer.setSeverity(slots[200], 0.9, 1, 0.35, 1, 10);
    layer.setSeverity(slots[203], 0.5, 0.5, 0, 0, 10);
    expect(layer.pendingUploadFloats()).toBe(4 * STRIDE);
    layer.flush();
    expect(buf(layer).data.updateRanges).toEqual([{ start: 200 * STRIDE, count: 4 * STRIDE }]);
  });

  it("starts colour transitions from the colour currently on screen", () => {
    const layer = new ArcLayer({ capacity: 4, transitionSeconds: 1 });
    const s = layer.alloc();
    layer.write(s, { ...seg(0), sevStart: 0, sevEnd: 0 });
    layer.setSeverity(s, 1, 1, 0, 0, 10); // 0 -> 1 starting at t=10
    layer.setSeverity(s, 0, 0, 0, 0, 10.5); // interrupted halfway
    const d = buf(layer).data.array;
    const o = s * STRIDE;
    expect(d[o + 14]).toBeCloseTo(0.5, 5); // aSevPrev = displayed value at t=10.5 (smoothstep(0.5) = 0.5)
    expect(d[o + 12]).toBe(0);
    expect(d[o + 19]).toBe(10.5);
  });

  it("grows past its capacity without losing data", () => {
    const layer = new ArcLayer({ capacity: 16 });
    for (let i = 0; i < 40; i++) layer.write(layer.alloc(), seg(i));
    const d = buf(layer).data.array;
    expect(d.length).toBeGreaterThanOrEqual(40 * STRIDE);
    expect(d[0]).toBeCloseTo(seg(0).p0[0]);
    expect(d[39 * STRIDE + 22]).toBe(0.5); // phase of the last segment
  });
});

describe("HopLayer", () => {
  it("is one Points object with recycled slots and ranged uploads", () => {
    const hops = new HopLayer(8);
    const a = hops.alloc();
    const b = hops.alloc();
    hops.write(a, latLonToVec3(0, 0), 0.2, false);
    hops.write(b, latLonToVec3(10, 10), 0.9, true);
    hops.update(0, 1);
    hops.release(a);
    expect(hops.isVisible(a)).toBe(false);
    expect(hops.alloc()).toBe(a);
    for (let i = 0; i < 20; i++) hops.write(hops.alloc(), latLonToVec3(i, i), 0, false);
    expect(hops.slotCount).toBe(22);
    expect(hops.positions[b * 3 + 1]).toBeCloseTo(Math.sin((10 * Math.PI) / 180) * 1.003, 6);
  });
});
