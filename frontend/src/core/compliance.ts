/**
 * Compliance status codes shared by the CPU and the shaders (Business Mode).
 * The engine (backend/internal/compliance) reports a status per route point;
 * the globe stores it as a number so the GPU can interpolate along arcs.
 */
export const COMPLIANCE = {
  not_evaluated: 0, // no enabled profile governs this traffic
  compliant: 1,
  suspected: 2, // outside the permitted territory, but the location is uncertain or an exception applies
  violation: 3,
} as const;

export type ComplianceStatus = keyof typeof COMPLIANCE;

export function statusCode(s: string | undefined): number {
  return COMPLIANCE[(s ?? "not_evaluated") as ComplianceStatus] ?? 0;
}

/** Worst of two codes. */
export function worse(a: number, b: number): number {
  return Math.max(a, b);
}

/** [code, r, g, b]: grey, teal, amber, red. */
export const COMPLIANCE_STOPS: ReadonlyArray<[number, number, number, number]> = [
  [0, 0.32, 0.4, 0.5],
  [1, 0.12, 0.82, 0.92],
  [2, 1.0, 0.72, 0.1],
  [3, 1.0, 0.1, 0.12],
];

const f = (n: number) => n.toFixed(4);

/** GLSL `vec3 complianceColor(float code)` from COMPLIANCE_STOPS. */
export const GLSL_COMPLIANCE = (() => {
  const lines = ["vec3 complianceColor(float c) {", "  c = clamp(c, 0.0, 3.0);"];
  for (let i = 1; i < COMPLIANCE_STOPS.length; i++) {
    const [p0, r0, g0, b0] = COMPLIANCE_STOPS[i - 1];
    const [p1, r1, g1, b1] = COMPLIANCE_STOPS[i];
    const cond = i < COMPLIANCE_STOPS.length - 1 ? `if (c <= ${f(p1)}) ` : "";
    lines.push(`  ${cond}return mix(vec3(${f(r0)}, ${f(g0)}, ${f(b0)}), vec3(${f(r1)}, ${f(g1)}, ${f(b1)}), c - ${f(p0)});`);
  }
  lines.push("}");
  return lines.join("\n");
})();

export function complianceCss(code: number): string {
  const [, r, g, b] = COMPLIANCE_STOPS[Math.max(0, Math.min(3, Math.round(code)))];
  return `rgb(${Math.round(r * 255)}, ${Math.round(g * 255)}, ${Math.round(b * 255)})`;
}
