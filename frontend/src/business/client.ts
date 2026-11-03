/**
 * Client for the compliance service (backend/cmd/complianced). In dev, Vite
 * proxies /v1/compliance to :8090; in production serve the globe behind the
 * same origin or start complianced with -cors-origins.
 */
import type { GeoJSONFeatureCollection } from "../render/Earth";
import type { Evaluation, Packet } from "./packet";

export interface Profile {
  id: string;
  name: string;
  enabled: boolean;
  classifications: string[];
  mode: "allow_list" | "deny_list";
  regions: string[];
  exceptions?: { region: string; effect: "allow" | "downgrade"; reason: string }[];
  scope: "all_hops" | "destination_only";
  severity: string;
  law: { code: string; citation: string };
  version: number;
}

export interface Region {
  id: string;
  name: string;
  version: number;
}

export interface LawEntry {
  iso2: string;
  name: string;
  laws: { short: string; name: string; citation: string; in_force?: string; type?: string; note?: string }[];
  localization?: { level: string; notes?: string };
  transfer_regime?: string;
  regulator?: string;
  dla_piper_url?: string | null;
  risk_flags?: string[];
}

export class ComplianceClient {
  constructor(
    private base = "",
    private token?: string,
    private timeoutMs = 15000,
  ) {}

  private async req<T>(path: string, init: RequestInit = {}): Promise<T> {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), this.timeoutMs);
    try {
      const headers: Record<string, string> = { Accept: "application/json" };
      if (init.body) headers["Content-Type"] = "application/json";
      if (this.token) headers.Authorization = `Bearer ${this.token}`;
      const res = await fetch(this.base + path, { ...init, headers, signal: ctl.signal });
      const text = await res.text();
      let body: unknown;
      try {
        body = text ? JSON.parse(text) : {};
      } catch {
        throw new Error(`compliance service: ${res.status} ${res.statusText} (not JSON - is complianced running?)`);
      }
      if (!res.ok) throw new Error(`compliance service: ${(body as { error?: string }).error ?? res.statusText}`);
      return body as T;
    } finally {
      clearTimeout(timer);
    }
  }

  profiles(): Promise<{ profiles: Profile[]; registry_snapshot: number }> {
    return this.req("/v1/compliance/profiles");
  }

  regions(): Promise<{ regions: Region[] }> {
    return this.req("/v1/compliance/regions");
  }

  regionGeometry(id: string, toleranceDeg = 0.05): Promise<GeoJSONFeatureCollection & { version: number }> {
    return this.req(`/v1/compliance/regions/${encodeURIComponent(id)}/geometry?tolerance=${toleranceDeg}`);
  }

  /** Preview evaluation: alerts are returned but not published to the alert stream. */
  evaluateBatch(packets: Packet[]): Promise<{ evaluations: Evaluation[]; errors: { index: number; packet_id: string; error: string }[]; registry_snapshot: number }> {
    return this.req("/v1/compliance/evaluate/batch", { method: "POST", body: JSON.stringify({ packets, publish: false }) });
  }

  laws(countries: string[]): Promise<{ as_of: string; jurisdictions: LawEntry[]; not_in_directory: string[] }> {
    return this.req(`/v1/compliance/laws?countries=${encodeURIComponent(countries.join(","))}`);
  }
}
