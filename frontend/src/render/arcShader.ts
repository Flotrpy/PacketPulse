/**
 * Instanced arc shader. Each instance is one hop-to-hop segment. The base
 * geometry is a flat strip: position.x = t along the curve (0..1),
 * position.y = side (-1 | +1). The vertex shader evaluates the cubic Bezier
 * from four per-instance control points and extrudes the strip in screen
 * space, so lines keep a constant pixel width at any zoom.
 */
import { GLSL_HEALTH } from "../core/health";

/** Medium codes written into aStyle.w. */
export const MEDIUM_CODE: Record<string, number> = {
  unknown: 0,
  lan: 1,
  cellular_backhaul: 2,
  metro_fiber: 3,
  terrestrial_fiber: 4,
  subsea_cable: 5,
  cloud_backbone: 6,
};

export const arcVertexShader = /* glsl */ `
attribute vec3 aP0;
attribute vec3 aP1;
attribute vec3 aP2;
attribute vec3 aP3;
attribute vec2 aSev;      // severity at segment start / end (target)
attribute vec2 aSevPrev;  // severity before the last update (for smooth transitions)
attribute vec2 aFlash;    // loss flash weight at start / end
attribute vec4 aTiming;   // birthTime, severityChangeTime, segmentIndex, segmentCount
attribute vec4 aStyle;    // phase, highlight, visible, medium

uniform float uTime;
uniform float uWidth;        // px
uniform float uDrawIn;       // seconds for a new path to draw in
uniform float uTransition;   // seconds for colour transitions
uniform vec2 uResolution;    // drawing-buffer px

varying float vT;
varying float vSide;
varying float vSev;
varying float vFlash;
varying float vGlobal;
varying float vReveal;
varying float vHighlight;
varying float vMedium;
varying float vPhase;
varying float vSegCount;

vec3 bez(float t) {
  float u = 1.0 - t;
  return u * u * u * aP0 + 3.0 * u * u * t * aP1 + 3.0 * u * t * t * aP2 + t * t * t * aP3;
}

void main() {
  if (aStyle.z < 0.5) {
    gl_Position = vec4(2.0, 2.0, 2.0, 1.0); // freed slot: outside clip space
    return;
  }
  float t = position.x;
  float side = position.y;

  mat4 mvp = projectionMatrix * modelViewMatrix;
  vec4 c  = mvp * vec4(bez(t), 1.0);
  vec4 ca = mvp * vec4(bez(max(t - 0.01, 0.0)), 1.0);
  vec4 cb = mvp * vec4(bez(min(t + 0.01, 1.0)), 1.0);

  vec2 sa = ca.xy / ca.w * uResolution;
  vec2 sb = cb.xy / cb.w * uResolution;
  vec2 dir = sb - sa;
  float len = length(dir);
  dir = len > 1e-5 ? dir / len : vec2(1.0, 0.0);
  vec2 nrm = vec2(-dir.y, dir.x);

  float w = uWidth * (1.0 + 0.9 * aStyle.y);
  c.xy += nrm * side * w / uResolution * c.w;
  gl_Position = c;

  float k = clamp((uTime - aTiming.y) / uTransition, 0.0, 1.0);
  k = k * k * (3.0 - 2.0 * k);
  vec2 sev = mix(aSevPrev, aSev, k);

  vT = t;
  vSide = side;
  vSev = mix(sev.x, sev.y, t);
  vFlash = mix(aFlash.x, aFlash.y, t);
  vSegCount = max(aTiming.w, 1.0);
  vGlobal = (aTiming.z + t) / vSegCount;
  vReveal = clamp((uTime - aTiming.x) / uDrawIn * vSegCount - aTiming.z, 0.0, 1.0);
  vHighlight = aStyle.y;
  vMedium = aStyle.w;
  vPhase = aStyle.x;
}
`;

export const arcFragmentShader = /* glsl */ `
uniform float uTime;
uniform float uCycle;        // seconds for a packet pulse to traverse a whole path
uniform float uFocusActive;  // 1 when a path is selected (others dim)

varying float vT;
varying float vSide;
varying float vSev;
varying float vFlash;
varying float vGlobal;
varying float vReveal;
varying float vHighlight;
varying float vMedium;
varying float vPhase;
varying float vSegCount;

${GLSL_HEALTH}

void main() {
  if (vT > vReveal) discard;

  float edge = 1.0 - abs(vSide);
  float core = pow(edge, 4.0);
  float glow = pow(edge, 1.3);

  vec3 col = severityColor(vSev);

  // Heavy loss: pulse towards hot red.
  float flash = vFlash * (0.5 + 0.5 * sin(uTime * 13.0 + vPhase * 6.2831));
  col = mix(col, vec3(1.0, 0.06, 0.06), clamp(flash, 0.0, 1.0) * 0.85);

  // Packet pulse travelling along the whole path; bright head, fading tail.
  float head = fract(uTime / uCycle + vPhase);
  float behind = head - vGlobal;
  float comet = behind >= 0.0 ? exp(-behind * 28.0) : exp(-(behind + 1.0) * 28.0);

  // Medium patterns: cellular = dotted, unknown = dashed (uncertain link).
  float pattern = 1.0;
  if (vMedium > 1.5 && vMedium < 2.5) pattern = 0.35 + 0.65 * step(0.45, fract(vT * 60.0));
  else if (vMedium < 0.5) pattern = 0.25 + 0.75 * step(0.4, fract(vT * 14.0));

  float dim = mix(1.0, mix(0.18, 1.0, vHighlight), uFocusActive);
  // Additive blending: keep the base line dim enough that overlapping arcs
  // keep their hue instead of saturating to white; the packet head carries
  // the bright highlight.
  float intensity = (0.22 * glow + 0.55 * core) * pattern + comet * (0.5 * glow + 1.1 * core);
  vec3 rgb = col * intensity + vec3(1.0) * comet * core * 0.35;
  float alpha = clamp((0.5 * glow + 0.8 * core) * pattern + comet * glow, 0.0, 1.0) * dim;
  gl_FragColor = vec4(rgb, alpha);
}
`;
