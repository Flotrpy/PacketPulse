/**
 * Deterministic traffic simulator for load-testing the globe without the
 * backend. Routes run between real network hub cities via intermediate hubs
 * near the great circle. RTTs follow fiber physics (204 km/ms, route stretch
 * 1.2-1.6x) plus per-hop processing. Congestion and loss events come and go,
 * so colours shift live; a share of paths is re-routed each tick to exercise
 * slot reuse and the draw-in animation.
 */
import { angleBetween, EARTH_RADIUS_KM, FIBER_KM_PER_MS, latLonToVec3, type Vec3 } from "../core/geo";
import type { HopInput, Medium, PathInput } from "../core/types";

interface Hub {
  city: string;
  cc: string;
  lat: number;
  lon: number;
  region: "na" | "sa" | "eu" | "af" | "me" | "as" | "oc";
}

// Major interconnection hubs (city-centre coordinates).
export const HUBS: Hub[] = [
  { city: "Ashburn", cc: "US", lat: 39.0438, lon: -77.4874, region: "na" },
  { city: "New York", cc: "US", lat: 40.7128, lon: -74.006, region: "na" },
  { city: "Chicago", cc: "US", lat: 41.8781, lon: -87.6298, region: "na" },
  { city: "Dallas", cc: "US", lat: 32.7767, lon: -96.797, region: "na" },
  { city: "Los Angeles", cc: "US", lat: 34.0522, lon: -118.2437, region: "na" },
  { city: "San Jose", cc: "US", lat: 37.3382, lon: -121.8863, region: "na" },
  { city: "Seattle", cc: "US", lat: 47.6062, lon: -122.3321, region: "na" },
  { city: "Miami", cc: "US", lat: 25.7617, lon: -80.1918, region: "na" },
  { city: "Toronto", cc: "CA", lat: 43.6532, lon: -79.3832, region: "na" },
  { city: "Mexico City", cc: "MX", lat: 19.4326, lon: -99.1332, region: "na" },
  { city: "Sao Paulo", cc: "BR", lat: -23.5505, lon: -46.6333, region: "sa" },
  { city: "Fortaleza", cc: "BR", lat: -3.7319, lon: -38.5267, region: "sa" },
  { city: "Buenos Aires", cc: "AR", lat: -34.6037, lon: -58.3816, region: "sa" },
  { city: "Santiago", cc: "CL", lat: -33.4489, lon: -70.6693, region: "sa" },
  { city: "Bogota", cc: "CO", lat: 4.711, lon: -74.0721, region: "sa" },
  { city: "London", cc: "GB", lat: 51.5074, lon: -0.1278, region: "eu" },
  { city: "Amsterdam", cc: "NL", lat: 52.3676, lon: 4.9041, region: "eu" },
  { city: "Frankfurt", cc: "DE", lat: 50.1109, lon: 8.6821, region: "eu" },
  { city: "Paris", cc: "FR", lat: 48.8566, lon: 2.3522, region: "eu" },
  { city: "Marseille", cc: "FR", lat: 43.2965, lon: 5.3698, region: "eu" },
  { city: "Madrid", cc: "ES", lat: 40.4168, lon: -3.7038, region: "eu" },
  { city: "Milan", cc: "IT", lat: 45.4642, lon: 9.19, region: "eu" },
  { city: "Stockholm", cc: "SE", lat: 59.3293, lon: 18.0686, region: "eu" },
  { city: "Warsaw", cc: "PL", lat: 52.2297, lon: 21.0122, region: "eu" },
  { city: "Istanbul", cc: "TR", lat: 41.0082, lon: 28.9784, region: "eu" },
  { city: "Moscow", cc: "RU", lat: 55.7558, lon: 37.6173, region: "eu" },
  { city: "Lagos", cc: "NG", lat: 6.5244, lon: 3.3792, region: "af" },
  { city: "Johannesburg", cc: "ZA", lat: -26.2041, lon: 28.0473, region: "af" },
  { city: "Cape Town", cc: "ZA", lat: -33.9249, lon: 18.4241, region: "af" },
  { city: "Nairobi", cc: "KE", lat: -1.2921, lon: 36.8219, region: "af" },
  { city: "Cairo", cc: "EG", lat: 30.0444, lon: 31.2357, region: "af" },
  { city: "Djibouti", cc: "DJ", lat: 11.5721, lon: 43.1456, region: "af" },
  { city: "Dubai", cc: "AE", lat: 25.2048, lon: 55.2708, region: "me" },
  { city: "Jeddah", cc: "SA", lat: 21.4858, lon: 39.1925, region: "me" },
  { city: "Tel Aviv", cc: "IL", lat: 32.0853, lon: 34.7818, region: "me" },
  { city: "Mumbai", cc: "IN", lat: 19.076, lon: 72.8777, region: "as" },
  { city: "Chennai", cc: "IN", lat: 13.0827, lon: 80.2707, region: "as" },
  { city: "Delhi", cc: "IN", lat: 28.7041, lon: 77.1025, region: "as" },
  { city: "Singapore", cc: "SG", lat: 1.3521, lon: 103.8198, region: "as" },
  { city: "Jakarta", cc: "ID", lat: -6.2088, lon: 106.8456, region: "as" },
  { city: "Hong Kong", cc: "HK", lat: 22.3193, lon: 114.1694, region: "as" },
  { city: "Shanghai", cc: "CN", lat: 31.2304, lon: 121.4737, region: "as" },
  { city: "Tokyo", cc: "JP", lat: 35.6762, lon: 139.6503, region: "as" },
  { city: "Seoul", cc: "KR", lat: 37.5665, lon: 126.978, region: "as" },
  { city: "Taipei", cc: "TW", lat: 25.033, lon: 121.5654, region: "as" },
  { city: "Manila", cc: "PH", lat: 14.5995, lon: 120.9842, region: "as" },
  { city: "Sydney", cc: "AU", lat: -33.8688, lon: 151.2093, region: "oc" },
  { city: "Perth", cc: "AU", lat: -31.9505, lon: 115.8605, region: "oc" },
  { city: "Auckland", cc: "NZ", lat: -36.8485, lon: 174.7633, region: "oc" },
];

const ISLANDS = new Set(["GB", "JP", "AU", "NZ", "TW", "PH", "ID", "KR", "SG"]);
const LAND_LINKED = new Set(["na|sa", "eu|as", "eu|me", "me|as", "af|me", "eu|eu"]);

export function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

interface SimHop {
  hub: Hub;
  dir: Vec3;
  baseDeltaMs: number; // propagation + processing for the link into this hop
  medium: Medium;
  extraMs: number; // congestion queueing delay
  jitterMs: number;
  lossPct: number;
  event: "none" | "congested" | "lossy";
  eventUntil: number;
}

interface SimPath {
  id: string;
  hops: SimHop[];
}

export interface SimulatorOptions {
  seed?: number;
  /** probability per path per second that an incident starts */
  incidentRate?: number;
  /** fraction of paths re-routed per second */
  churnRate?: number;
}

export class Simulator {
  private rnd: () => number;
  private paths = new Map<string, SimPath>();
  private seq = 0;
  private opts: Required<SimulatorOptions>;

  constructor(opts: SimulatorOptions = {}) {
    this.opts = { seed: opts.seed ?? 42, incidentRate: opts.incidentRate ?? 0.08, churnRate: opts.churnRate ?? 0.01 };
    this.rnd = mulberry32(this.opts.seed);
  }

  get size(): number {
    return this.paths.size;
  }

  /** Create n new paths; returns them as globe inputs. */
  spawn(n: number): PathInput[] {
    const out: PathInput[] = [];
    for (let i = 0; i < n; i++) {
      const p = this.makePath();
      this.paths.set(p.id, p);
      out.push(this.toInput(p));
    }
    return out;
  }

  /**
   * Advance the simulation to `now` (seconds) by `dt`. Returns paths whose
   * metrics changed and ids of paths removed by churn.
   */
  step(now: number, dt: number): { updated: PathInput[]; removed: string[] } {
    const updated: PathInput[] = [];
    const removed: string[] = [];
    const churnCount = Math.floor(this.paths.size * this.opts.churnRate * dt + this.rnd());
    const ids = [...this.paths.keys()];
    for (let i = 0; i < churnCount && ids.length > 0; i++) {
      const id = ids.splice(Math.floor(this.rnd() * ids.length), 1)[0];
      this.paths.delete(id);
      removed.push(id);
      const p = this.makePath();
      this.paths.set(p.id, p);
      updated.push(this.toInput(p));
    }
    for (const p of this.paths.values()) {
      let changed = false;
      for (const h of p.hops.slice(1)) {
        if (h.event !== "none" && now >= h.eventUntil) {
          h.event = "none";
          h.extraMs = 0;
          h.lossPct = 0;
          h.jitterMs = 0.3 + this.rnd() * 2;
          changed = true;
        }
      }
      if (this.rnd() < this.opts.incidentRate * dt) {
        const h = p.hops[1 + Math.floor(this.rnd() * (p.hops.length - 1))];
        if (this.rnd() < 0.65) {
          h.event = "congested";
          h.extraMs = h.baseDeltaMs * (1.5 + this.rnd() * 3) + 5;
          h.jitterMs = 8 + this.rnd() * 30;
        } else {
          h.event = "lossy";
          h.lossPct = 2 + this.rnd() * 18;
          h.extraMs = h.baseDeltaMs * this.rnd();
          h.jitterMs = 15 + this.rnd() * 40;
        }
        h.eventUntil = now + 3 + this.rnd() * 8;
        changed = true;
      }
      if (changed) updated.push(this.toInput(p));
    }
    return { updated, removed };
  }

  private makePath(): SimPath {
    const src = HUBS[Math.floor(this.rnd() * HUBS.length)];
    let dst = src;
    while (dst === src) dst = HUBS[Math.floor(this.rnd() * HUBS.length)];
    const a = latLonToVec3(src.lat, src.lon);
    const b = latLonToVec3(dst.lat, dst.lon);
    const direct = angleBetween(a, b);

    // Intermediate hubs near the great circle, ordered along it.
    const via = HUBS.filter((h) => h !== src && h !== dst)
      .map((h) => {
        const v = latLonToVec3(h.lat, h.lon);
        return { h, v, along: angleBetween(a, v), detour: angleBetween(a, v) + angleBetween(v, b) - direct };
      })
      .filter((c) => c.detour < 0.04 + direct * 0.12 && this.rnd() < 0.6)
      .sort((x, y) => x.along - y.along)
      .slice(0, 4);

    // Access hop: a metro router ~10-40 km from the source city (sometimes a cellular gateway).
    const cellular = this.rnd() < 0.3;
    const access: Hub = { ...src, lat: src.lat + (this.rnd() - 0.5) * 0.5, lon: src.lon + (this.rnd() - 0.5) * 0.5 };
    const chain: Hub[] = [src, access, ...via.map((c) => c.h), dst];

    const hops: SimHop[] = chain.map((hub, i) => {
      const dir = latLonToVec3(hub.lat, hub.lon);
      let base = 0;
      let medium: Medium = "unknown";
      if (i > 0) {
        const prev = chain[i - 1];
        const km = angleBetween(latLonToVec3(prev.lat, prev.lon), dir) * EARTH_RADIUS_KM;
        base = ((2 * km) / FIBER_KM_PER_MS) * (1.2 + this.rnd() * 0.4) + 0.3 + this.rnd() * 0.8;
        medium = i === 1 ? (cellular ? "cellular_backhaul" : "metro_fiber") : this.mediumFor(prev, hub, km);
        if (i === 1 && cellular) base += 12 + this.rnd() * 18; // radio access latency
      }
      return { hub, dir, baseDeltaMs: base, medium, extraMs: 0, jitterMs: 0.3 + this.rnd() * 2, lossPct: 0, event: "none", eventUntil: 0 };
    });
    return { id: `sim-${this.opts.seed}-${++this.seq}`, hops };
  }

  private mediumFor(a: Hub, b: Hub, km: number): Medium {
    if (km < 60) return "metro_fiber";
    if (a.cc !== b.cc && (ISLANDS.has(a.cc) || ISLANDS.has(b.cc))) return "subsea_cable";
    const key = [a.region, b.region].sort().join("|");
    if (a.region === b.region || LAND_LINKED.has(key)) return km > 4500 ? "subsea_cable" : "terrestrial_fiber";
    return "subsea_cable";
  }

  private toInput(p: SimPath): PathInput {
    let rtt = 0;
    const hops: HopInput[] = p.hops.map((h, i) => {
      if (i > 0) rtt += h.baseDeltaMs + h.extraMs;
      return {
        lat: h.hub.lat,
        lon: h.hub.lon,
        rttMs: Math.round(rtt * 100) / 100,
        jitterMs: Math.round(h.jitterMs * 100) / 100,
        lossPct: Math.round(h.lossPct * 10) / 10,
        medium: i > 0 ? h.medium : undefined,
        hopNumber: i,
        city: h.hub.city,
        country: h.hub.cc,
      };
    });
    const last = p.hops[p.hops.length - 1].hub;
    return { id: p.id, hops, label: `${p.hops[0].hub.city} → ${last.city}` };
  }
}
