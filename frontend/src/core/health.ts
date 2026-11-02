/**
 * Latency -> colour mapping.
 *
 * Each hop gets a severity in [0, 1] from three metrics, using the same
 * green/amber thresholds as the backend's segment health:
 *   latency excess  = delta RTT / speed-of-light RTT for the hop distance (green < 2.0, amber < 3.5)
 *   jitter (ms)                                                           (green < 10,  amber < 30)
 *   loss (%)                                                              (green < 0.5, amber < 2)
 * Severity is the worst of the three, mapped piecewise so status bands land
 * on colour plateaus: green [0, 0.35], amber (0.35, 0.8], red (0.8, 1].
 *
 * The gradient (GRADIENT_STOPS) is shared with the GLSL in arcShader.ts via
 * GLSL_HEALTH so CPU-coloured hop markers match the GPU-coloured arcs.
 */
import { FIBER_KM_PER_MS } from "./geo";

export interface Thresholds {
  green: number;
  amber: number;
}

export const LATENCY_EXCESS: Thresholds = { green: 2.0, amber: 3.5 };
export const JITTER_MS: Thresholds = { green: 10, amber: 30 };
export const LOSS_PCT: Thresholds = { green: 0.5, amber: 2 };

/** Loss at or above this flashes red. */
export const FLASH_LOSS_PCT = LOSS_PCT.amber;

/** Short hops: RTT added beyond this many ms counts as excess even when the distance is tiny. */
const SHORT_HOP_FLOOR_MS = 5;

export function metricSeverity(value: number, t: Thresholds): number {
  if (!(value > 0)) return 0;
  if (value <= t.green) return 0.35 * (value / t.green);
  if (value <= t.amber) return 0.35 + 0.45 * ((value - t.green) / (t.amber - t.green));
  return Math.min(1, 0.8 + 0.2 * ((value - t.amber) / t.amber));
}

export interface HopMetrics {
  /** min RTT from the source to this hop (ms) */
  rttMs: number;
  jitterMs?: number;
  lossPct?: number;
}

export interface SegmentHealth {
  severity: number;
  flash: boolean;
  latencyExcess: number;
}

/**
 * Health of the link that ends at `to`, given the previous hop's RTT and the
 * great-circle distance between them.
 */
export function segmentHealth(from: HopMetrics | null, to: HopMetrics, distanceKm: number): SegmentHealth {
  const delta = Math.max(0, to.rttMs - (from?.rttMs ?? 0));
  const feasible = (2 * distanceKm) / FIBER_KM_PER_MS;
  const excess = feasible >= 1 ? delta / feasible : delta > SHORT_HOP_FLOOR_MS ? delta / SHORT_HOP_FLOOR_MS : 1;
  const severity = Math.max(
    metricSeverity(excess, LATENCY_EXCESS),
    metricSeverity(to.jitterMs ?? 0, JITTER_MS),
    metricSeverity(to.lossPct ?? 0, LOSS_PCT),
  );
  return { severity, flash: (to.lossPct ?? 0) >= FLASH_LOSS_PCT, latencyExcess: excess };
}

export type RGB = [number, number, number];

/** Gradient stops [position, r, g, b] (linear-ish sRGB, 0..1). */
export const GRADIENT_STOPS: ReadonlyArray<[number, number, number, number]> = [
  [0.0, 0.1, 1.0, 0.55], // glowing green
  [0.35, 0.45, 1.0, 0.3], // yellow-green edge of "healthy"
  [0.55, 1.0, 0.7, 0.12], // amber
  [0.8, 1.0, 0.45, 0.08], // deep amber
  [1.0, 1.0, 0.12, 0.12], // red
];

export function severityColor(s: number, out: RGB = [0, 0, 0]): RGB {
  const x = Math.max(0, Math.min(1, s));
  for (let i = 1; i < GRADIENT_STOPS.length; i++) {
    const [p1, r1, g1, b1] = GRADIENT_STOPS[i];
    if (x <= p1) {
      const [p0, r0, g0, b0] = GRADIENT_STOPS[i - 1];
      const k = (x - p0) / (p1 - p0);
      out[0] = r0 + (r1 - r0) * k;
      out[1] = g0 + (g1 - g0) * k;
      out[2] = b0 + (b1 - b0) * k;
      return out;
    }
  }
  const last = GRADIENT_STOPS[GRADIENT_STOPS.length - 1];
  out[0] = last[1];
  out[1] = last[2];
  out[2] = last[3];
  return out;
}

export function statusOf(severity: number): "green" | "amber" | "red" {
  return severity <= 0.35 ? "green" : severity <= 0.8 ? "amber" : "red";
}

const f = (n: number) => n.toFixed(4);

/** GLSL `vec3 severityColor(float s)` generated from GRADIENT_STOPS. */
export const GLSL_HEALTH = (() => {
  const lines = ["vec3 severityColor(float s) {", "  s = clamp(s, 0.0, 1.0);"];
  for (let i = 1; i < GRADIENT_STOPS.length; i++) {
    const [p0, r0, g0, b0] = GRADIENT_STOPS[i - 1];
    const [p1, r1, g1, b1] = GRADIENT_STOPS[i];
    const cond = i < GRADIENT_STOPS.length - 1 ? `if (s <= ${f(p1)}) ` : "";
    lines.push(
      `  ${cond}return mix(vec3(${f(r0)}, ${f(g0)}, ${f(b0)}), vec3(${f(r1)}, ${f(g1)}, ${f(b1)}), (s - ${f(p0)}) / ${f(p1 - p0)});`,
    );
  }
  lines.push("}");
  return lines.join("\n");
})();
