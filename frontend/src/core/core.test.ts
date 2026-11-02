import { describe, expect, it } from "vitest";
import { arcControls, arcHeight, bezier, SURFACE_LIFT } from "./curve";
import { angleBetween, haversineKm, latLonToVec3, minFeasibleRttMs, slerp, vec3ToLatLon } from "./geo";
import { GLSL_HEALTH, metricSeverity, segmentHealth, severityColor, statusOf } from "./health";

const MUMBAI = { lat: 19.076, lon: 72.8777 };
const MARSEILLE = { lat: 43.2965, lon: 5.3698 };

describe("geo", () => {
  it("round-trips lat/lon", () => {
    for (const [lat, lon] of [[0, 0], [51.5, -0.12], [-33.9, 151.2], [85, 179.9], [-85, -179.9]]) {
      const back = vec3ToLatLon(latLonToVec3(lat, lon));
      expect(back.lat).toBeCloseTo(lat, 9);
      expect(back.lon).toBeCloseTo(lon, 9);
    }
  });

  it("uses Y-up with lon 0 facing +Z and lon 90 facing +X", () => {
    expect(latLonToVec3(0, 0)).toEqual([0, 0, 1]);
    const e = latLonToVec3(0, 90);
    expect(e[0]).toBeCloseTo(1);
    expect(latLonToVec3(90, 0)[1]).toBeCloseTo(1);
  });

  it("matches the backend's Mumbai-Marseille distance", () => {
    // backend/internal/pipeline reported 6,764.7 km for these coordinates
    expect(haversineKm(19.07283, 72.88261, 43.29695, 5.38107)).toBeCloseTo(6764.7, 0);
    expect(minFeasibleRttMs(6764.7)).toBeCloseTo(66.26, 1);
  });

  it("slerp stays on the unit sphere, including antipodes", () => {
    const a = latLonToVec3(10, 20);
    const b = latLonToVec3(-10, -160); // antipode
    for (const t of [0, 0.25, 0.5, 0.75, 1]) {
      const p = slerp(a, b, t);
      expect(Math.hypot(...p)).toBeCloseTo(1, 9);
    }
    expect(angleBetween(slerp(a, b, 1), b)).toBeLessThan(1e-6);
    expect(angleBetween(slerp(a, b, 0.5), a)).toBeCloseTo(Math.PI / 2, 6);
  });
});

describe("arc curve", () => {
  it("starts and ends at the hops, arches above the surface in between", () => {
    const a = latLonToVec3(MUMBAI.lat, MUMBAI.lon);
    const b = latLonToVec3(MARSEILLE.lat, MARSEILLE.lon);
    const c = arcControls(a, b);
    expect(bezier(c, 0)).toEqual(c.p0);
    expect(Math.hypot(...bezier(c, 0))).toBeCloseTo(SURFACE_LIFT);
    let peak = 0;
    for (let t = 0; t <= 1; t += 0.05) {
      const r = Math.hypot(...bezier(c, t));
      expect(r).toBeGreaterThanOrEqual(SURFACE_LIFT - 1e-9);
      peak = Math.max(peak, r);
    }
    // Peak altitude matches the design height for this distance.
    expect(peak - 1).toBeCloseTo(arcHeight(angleBetween(a, b)), 3);
  });

  it("keeps metro hops flat and antipodal hops high", () => {
    expect(arcHeight(0.001)).toBeLessThan(0.015);
    expect(arcHeight(Math.PI)).toBeCloseTo(0.412, 3);
    // Antipodal arcs still peak at the design height (no chord collapse).
    const a = latLonToVec3(0, 0);
    const c = arcControls(a, latLonToVec3(0, 180));
    expect(Math.hypot(...bezier(c, 0.5)) - 1).toBeCloseTo(arcHeight(Math.PI), 3);
  });
});

describe("health colour mapping", () => {
  it("maps metric status bands onto colour plateaus", () => {
    const T = { green: 10, amber: 30 };
    expect(metricSeverity(0, T)).toBe(0);
    expect(metricSeverity(10, T)).toBeCloseTo(0.35);
    expect(metricSeverity(30, T)).toBeCloseTo(0.8);
    expect(metricSeverity(1000, T)).toBe(1);
  });

  it("scores a hop from latency excess, jitter and loss (worst wins)", () => {
    // Mumbai -> Marseille in 92.8 ms over 6,765 km: 1.4x light speed -> green
    const ok = segmentHealth({ rttMs: 19.6 }, { rttMs: 112.4, jitterMs: 3.2 }, 6764.7);
    expect(statusOf(ok.severity)).toBe("green");
    expect(ok.latencyExcess).toBeCloseTo(1.4, 1);
    // Congestion: 3x slower than light allows -> amber
    expect(statusOf(segmentHealth({ rttMs: 0 }, { rttMs: 200 }, 6764.7).severity)).toBe("amber");
    // Loss beyond 2% -> red and flashing even when latency is fine
    const lossy = segmentHealth({ rttMs: 19.6 }, { rttMs: 112.4, lossPct: 6 }, 6764.7);
    expect(statusOf(lossy.severity)).toBe("red");
    expect(lossy.flash).toBe(true);
    // A short hop adding 40 ms is congestion even though it is only 10 km
    expect(statusOf(segmentHealth({ rttMs: 1 }, { rttMs: 41 }, 10).severity)).toBe("red");
  });

  it("gradient runs green -> amber -> red", () => {
    const [r0, g0] = severityColor(0);
    const [r5, g5] = severityColor(0.55);
    const [r1, g1, b1] = severityColor(1);
    expect(g0).toBeGreaterThan(r0);
    expect(r5).toBeGreaterThan(0.9);
    expect(g5).toBeGreaterThan(0.5);
    expect(r1).toBeGreaterThan(0.9);
    expect(g1).toBeLessThan(0.2);
    expect(b1).toBeLessThan(0.2);
  });

  it("generates GLSL from the same stops", () => {
    expect(GLSL_HEALTH).toContain("vec3 severityColor(float s)");
    expect(GLSL_HEALTH.match(/return mix/g)?.length).toBe(4);
    expect(GLSL_HEALTH).toContain("vec3(1.0000, 0.1200, 0.1200)");
  });
});
