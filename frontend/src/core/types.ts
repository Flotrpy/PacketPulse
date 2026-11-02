/** Input model for the globe: one packet path = ordered, located hops. */

export type Medium =
  | "lan"
  | "cellular_backhaul"
  | "metro_fiber"
  | "terrestrial_fiber"
  | "subsea_cable"
  | "cloud_backbone"
  | "unknown";

export interface HopInput {
  lat: number;
  lon: number;
  /** min RTT from the source to this hop, ms (0 for the source itself) */
  rttMs: number;
  jitterMs?: number;
  lossPct?: number;
  /** transport medium of the link arriving at this hop */
  medium?: Medium;
  hopNumber?: number;
  ip?: string;
  hostname?: string;
  asn?: number;
  asOrg?: string;
  city?: string;
  country?: string;
  /** cable name when medium === "subsea_cable" */
  cable?: string;
  borderViolation?: boolean;
  /** 0..1 confidence in lat/lon (GeoIP ~0.7, router hostname ~0.85, device fix 1). */
  locationConfidence?: number;
  /** Cable landing stations crossed on the link arriving at this hop. */
  landings?: LandingInput[];
}

export interface LandingInput {
  name: string;
  country: string; // ISO2
  lat: number;
  lon: number;
  /** Probability that traffic actually lands here (cable-match certainty). */
  confidence: number;
}

export interface PathInput {
  id: string;
  hops: HopInput[];
  label?: string;
  verdict?: string;
  /** When the trace started (RFC 3339): compliance entry times are relative to it. */
  startedAt?: string;
  /** Data classifications assigned at ingestion (Business Mode evaluates the path with them). */
  classifications?: string[];
}

/** A hop the user picked. */
export interface HopSelection {
  pathId: string;
  hopIndex: number;
  hop: HopInput;
  severity: number;
  latencyExcess: number;
}
