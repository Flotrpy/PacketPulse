/**
 * Geographic helpers shared by the CPU (picking, camera) and mirrored in GLSL.
 *
 * Globe frame: unit sphere, Y up. Longitude 0 faces +Z, longitude +90 faces +X.
 *   x = cos(lat) sin(lon), y = sin(lat), z = cos(lat) cos(lon)
 */
export type Vec3 = [number, number, number];

export const EARTH_RADIUS_KM = 6371;
const DEG = Math.PI / 180;

export function latLonToVec3(lat: number, lon: number, radius = 1, out: Vec3 = [0, 0, 0]): Vec3 {
  const phi = lat * DEG;
  const lam = lon * DEG;
  const c = Math.cos(phi);
  out[0] = radius * c * Math.sin(lam);
  out[1] = radius * Math.sin(phi);
  out[2] = radius * c * Math.cos(lam);
  return out;
}

export function vec3ToLatLon(v: Vec3): { lat: number; lon: number } {
  const r = Math.hypot(v[0], v[1], v[2]) || 1;
  return { lat: Math.asin(Math.max(-1, Math.min(1, v[1] / r))) / DEG, lon: Math.atan2(v[0], v[2]) / DEG };
}

export function dot(a: Vec3, b: Vec3): number {
  return a[0] * b[0] + a[1] * b[1] + a[2] * b[2];
}

export function normalize(v: Vec3): Vec3 {
  const l = Math.hypot(v[0], v[1], v[2]) || 1;
  return [v[0] / l, v[1] / l, v[2] / l];
}

/** Angle between two unit vectors, robust near 0 and pi. */
export function angleBetween(a: Vec3, b: Vec3): number {
  const cx = a[1] * b[2] - a[2] * b[1];
  const cy = a[2] * b[0] - a[0] * b[2];
  const cz = a[0] * b[1] - a[1] * b[0];
  return Math.atan2(Math.hypot(cx, cy, cz), dot(a, b));
}

export function haversineKm(lat1: number, lon1: number, lat2: number, lon2: number): number {
  const a = latLonToVec3(lat1, lon1);
  const b = latLonToVec3(lat2, lon2);
  return angleBetween(a, b) * EARTH_RADIUS_KM;
}

/**
 * Spherical linear interpolation between unit vectors. For (nearly) antipodal
 * inputs the great circle is undefined; we pick one through the pole axis so
 * the result is deterministic (the shader does the same).
 */
export function slerp(a: Vec3, b: Vec3, t: number): Vec3 {
  const omega = angleBetween(a, b);
  if (omega < 1e-6) return [...a] as Vec3;
  if (Math.PI - omega < 1e-3) {
    // Antipodal: rotate through a perpendicular axis.
    const ref: Vec3 = Math.abs(a[1]) < 0.9 ? [0, 1, 0] : [1, 0, 0];
    const perp = normalize([ref[0] - a[0] * dot(a, ref), ref[1] - a[1] * dot(a, ref), ref[2] - a[2] * dot(a, ref)]);
    // a -> perp -> -a ; split the half circle in two quarter turns.
    if (t <= 0.5) return slerp(a, perp, t * 2);
    return slerp(perp, b, (t - 0.5) * 2);
  }
  const s = Math.sin(omega);
  const wa = Math.sin((1 - t) * omega) / s;
  const wb = Math.sin(t * omega) / s;
  return [a[0] * wa + b[0] * wb, a[1] * wa + b[1] * wb, a[2] * wa + b[2] * wb];
}

/** Light in fiber travels ~204.2 km per millisecond (c / 1.468). */
export const FIBER_KM_PER_MS = 204.2;

/** Round-trip time light needs over `km` of fiber. */
export function minFeasibleRttMs(km: number): number {
  return (2 * km) / FIBER_KM_PER_MS;
}
