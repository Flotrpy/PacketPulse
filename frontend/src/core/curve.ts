/**
 * Arched cubic Bezier between two points on the globe. The vertex shader
 * (render/arcShader.ts) evaluates exactly this curve on the GPU; this CPU
 * copy is used for picking, camera framing and tests.
 *
 * P0 = start on the surface, P3 = end on the surface.
 * P1, P2 sit above the great circle at 1/4 and 3/4 of the way. Their radius
 * is solved so the curve's midpoint peaks exactly at altitude arcHeight(angle):
 * the chord between distant endpoints sags inside the sphere, so naive
 * control points at 1 + h would arch far lower than h.
 */
import { angleBetween, slerp, type Vec3 } from "./geo";

export const SURFACE_LIFT = 1.002; // keep arcs just above the globe surface

/** Peak altitude (globe radii) for an arc spanning `angle` radians. */
export function arcHeight(angle: number): number {
  // 0.012 for adjacent cities, ~0.41 for antipodes.
  return 0.012 + 0.4 * Math.pow(angle / Math.PI, 0.85);
}

export interface ArcControls {
  p0: Vec3;
  p1: Vec3;
  p2: Vec3;
  p3: Vec3;
}

export function arcControls(a: Vec3, b: Vec3): ArcControls {
  const theta = angleBetween(a, b);
  const peak = 1 + arcHeight(theta);
  const c1 = slerp(a, b, 0.25);
  const c2 = slerp(a, b, 0.75);
  // B(0.5) = (P0 + 3 P1 + 3 P2 + P3) / 8; projected on the mid direction:
  // peak = (2 L cos(theta/2) + 6 R cos(theta/4)) / 8  ->  solve for R.
  const lift = (8 * peak - 2 * SURFACE_LIFT * Math.cos(theta / 2)) / (6 * Math.cos(theta / 4));
  return {
    p0: [a[0] * SURFACE_LIFT, a[1] * SURFACE_LIFT, a[2] * SURFACE_LIFT],
    p1: [c1[0] * lift, c1[1] * lift, c1[2] * lift],
    p2: [c2[0] * lift, c2[1] * lift, c2[2] * lift],
    p3: [b[0] * SURFACE_LIFT, b[1] * SURFACE_LIFT, b[2] * SURFACE_LIFT],
  };
}

export function bezier(c: ArcControls, t: number, out: Vec3 = [0, 0, 0]): Vec3 {
  const u = 1 - t;
  const w0 = u * u * u;
  const w1 = 3 * u * u * t;
  const w2 = 3 * u * t * t;
  const w3 = t * t * t;
  for (let i = 0; i < 3; i++) out[i] = w0 * c.p0[i] + w1 * c.p1[i] + w2 * c.p2[i] + w3 * c.p3[i];
  return out;
}
