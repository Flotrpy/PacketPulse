/**
 * "Trace from my computer": queues a trace on the user's own ppagent
 * (backend/cmd/ppagent) through /v1/agent/*, waits for the journey to come
 * back through the live stream, and flies the camera to it. The path starts
 * at the agent's real location and public IP.
 */
import type { PacketGlobe } from "../PacketGlobe";

interface AgentInfo {
  agent_id: string;
  public_ip?: string;
  as_org?: string;
  os?: string;
  tracing: boolean;
  online: boolean;
  last_seen: string;
  location?: { city?: string; country_iso2: string; lat: number; lon: number };
}
interface AgentJob {
  job_id: string;
  target: string;
  status: "queued" | "dispatched" | "done" | "failed";
  journey_id?: string;
  error?: string;
  ran_on?: string;
}

const TOKEN_KEY = "packetpulse.traceToken";

/**
 * Servers that live in one European data centre (not behind a CDN), so a
 * trace from the Americas has to cross the Atlantic on a subsea cable.
 */
export const EUROPE_TARGETS: { host: string; label: string }[] = [
  { host: "speedtest.london.linode.com", label: "London" },
  { host: "proof.ovh.net", label: "Roubaix" },
  { host: "speedtest.ams1.nl.leaseweb.net", label: "Amsterdam" },
  { host: "speedtest.frankfurt.linode.com", label: "Frankfurt" },
  { host: "www.kth.se", label: "Stockholm" },
  { host: "hel1-speed.hetzner.com", label: "Helsinki" },
];
const SERVER = "https://packetpulse-test.onrender.com"; // where ppagent connects (long-polls bypass the CDN)

function load(key: string): string {
  try {
    return localStorage.getItem(key) ?? "";
  } catch {
    return "";
  }
}
function save(key: string, v: string): void {
  try {
    localStorage.setItem(key, v);
  } catch {
    /* private mode: keep it in memory only */
  }
}
function esc(s: unknown): string {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);
}

export class TracePanel {
  private agents: AgentInfo[] = [];
  private busy = false;
  private timer = 0;

  constructor(
    private root: HTMLElement,
    private globe: PacketGlobe,
    private ensureLive: () => void,
  ) {
    this.render();
    this.root.addEventListener("submit", (e) => {
      e.preventDefault();
      void this.trace();
    });
    this.root.addEventListener("click", (e) => {
      const t = e.target as HTMLElement;
      if (t.dataset.act === "setup") this.root.querySelector(".tp-setup")?.classList.toggle("open");
      if (t.dataset.target) {
        const input = this.root.querySelector<HTMLInputElement>('input[name="target"]')!;
        input.value = t.dataset.target;
        void this.trace();
      }
    });
    void this.refreshAgents();
    this.timer = window.setInterval(() => void this.refreshAgents(), 10000);
  }

  dispose(): void {
    clearInterval(this.timer);
  }

  private async refreshAgents(): Promise<void> {
    try {
      const r = await fetch("/v1/agent/status", { cache: "no-store" });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      this.agents = ((await r.json()) as { agents: AgentInfo[] }).agents;
    } catch {
      this.agents = [];
    }
    this.renderAgent();
  }

  private online(): AgentInfo | undefined {
    return this.agents.find((a) => a.online);
  }

  private render(): void {
    const origin = location.origin;
    this.root.innerHTML = `
      <h2>Trace from my computer</h2>
      <div class="tp-agent muted">checking for your agent…</div>
      <form class="tp-form">
        <input name="target" placeholder="example.com or 1.1.1.1" autocomplete="off" required maxlength="253" />
        <input name="cls" placeholder="data class (optional, e.g. eu_personal_data)" autocomplete="off" maxlength="64" />
        <input name="token" type="password" placeholder="trace token" autocomplete="off" value="${esc(load(TOKEN_KEY))}" />
        <button type="submit">Trace</button>
      </form>
      <div class="tp-quick"><span class="muted">Across the Atlantic:</span> ${EUROPE_TARGETS.map((t) => `<button type="button" data-target="${esc(t.host)}" title="${esc(t.host)}">${esc(t.label)}</button>`).join("")}</div>
      <div class="tp-status muted"></div>
      <a href="#" data-act="setup" class="tp-link" onclick="return false">Set up the agent on this computer</a>
      <div class="tp-setup">
        <p>The agent traces from your network and public IP. It needs raw sockets, so run it with sudo / as Administrator.</p>
        <b>macOS (Apple silicon)</b>
        <pre>curl -fLo ppagent ${esc(origin)}/v1/agent/download/ppagent-darwin-arm64 && chmod +x ppagent
sudo PACKETPULSE_AGENT_TOKEN=&lt;agent token&gt; ./ppagent</pre>
        <b>macOS (Intel)</b> <span class="muted">ppagent-darwin-amd64</span> · <b>Linux</b> <span class="muted">ppagent-linux-amd64 / -arm64</span>
        <b>Windows</b> <span class="muted">(Start → type PowerShell → right-click → Run as administrator, then paste)</span>
        <pre>irm ${esc(SERVER)}/v1/agent/download/start-ppagent.ps1 | iex</pre>
        <p class="muted">It downloads the agent, opens Windows Firewall for traceroute replies, asks for your agent token once, and starts.</p>
        <p class="muted">It connects to ${esc(SERVER)}, finds your location from your public IP (override with -lat -lon -country), and waits for traces from this box.</p>
      </div>
      <div class="tp-attr muted"><a href="https://db-ip.com" target="_blank" rel="noopener">IP Geolocation by DB-IP</a> · Earth imagery: <a href="https://www.solarsystemscope.com/textures/" target="_blank" rel="noopener">Solar System Scope</a> (CC BY 4.0), NASA</div>`;
  }

  private renderAgent(): void {
    const el = this.root.querySelector(".tp-agent");
    if (!el) return;
    const a = this.online();
    if (!a) {
      el.className = "tp-agent warn";
      el.textContent = "No agent online. Start ppagent on your computer (setup below).";
      return;
    }
    const where = a.location ? [a.location.city, a.location.country_iso2].filter(Boolean).join(", ") : "location unknown";
    el.className = "tp-agent ok";
    el.innerHTML = `● <b>${esc(a.agent_id)}</b> online · ${esc(where)}${a.public_ip ? ` · ${esc(a.public_ip)}` : ""}${a.tracing ? "" : ' <span class="warn">(no raw sockets: run with sudo)</span>'}`;
  }

  private status(msg: string, cls = "muted"): void {
    const el = this.root.querySelector(".tp-status");
    if (el) {
      el.className = `tp-status ${cls}`;
      el.textContent = msg;
    }
  }

  private async trace(): Promise<void> {
    if (this.busy) return;
    const f = this.root.querySelector<HTMLFormElement>(".tp-form")!;
    const data = new FormData(f);
    const target = String(data.get("target") ?? "").trim();
    const cls = String(data.get("cls") ?? "").trim();
    const token = String(data.get("token") ?? "").trim();
    if (!token) return this.status("Enter the trace token.", "warn");
    save(TOKEN_KEY, token);
    this.ensureLive();
    this.busy = true;
    try {
      this.status(`queuing ${target}…`);
      const r = await fetch("/v1/agent/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
        body: JSON.stringify({ target, ...(cls ? { data_classifications: [cls] } : {}) }),
      });
      const body = (await r.json().catch(() => ({}))) as AgentJob & { error?: string };
      if (!r.ok) throw new Error(body.error ?? `HTTP ${r.status}`);
      const job = await this.wait(body.job_id, target);
      this.status(`${target}: path received, flying there…`, "ok");
      await this.focusWhenDrawn(job.journey_id!);
      this.status(`${target}: traced from ${job.ran_on}.`, "ok");
    } catch (err) {
      this.status(err instanceof Error ? err.message : String(err), "warn");
    } finally {
      this.busy = false;
    }
  }

  private async wait(id: string, target: string): Promise<AgentJob> {
    const t0 = Date.now();
    while (Date.now() - t0 < 180_000) {
      await new Promise((res) => setTimeout(res, 1200));
      const r = await fetch(`/v1/agent/jobs/${encodeURIComponent(id)}`, { cache: "no-store" });
      if (!r.ok) continue;
      const j = (await r.json()) as AgentJob;
      const s = Math.round((Date.now() - t0) / 1000);
      if (j.status === "done") return j;
      if (j.status === "failed") throw new Error(`${target}: ${j.error ?? "trace failed"}`);
      this.status(j.status === "queued" ? `waiting for your agent… ${s}s` : `tracing ${target} from ${j.ran_on}… ${s}s`);
    }
    throw new Error(`${target}: timed out`);
  }

  /** The journey arrives through the live stream; wait until it is on the globe. */
  private async focusWhenDrawn(journeyId: string): Promise<void> {
    for (let i = 0; i < 50; i++) {
      if (this.globe.getPath(journeyId)) {
        await this.globe.focusPath(journeyId);
        return;
      }
      await new Promise((res) => setTimeout(res, 300));
    }
    throw new Error("The trace finished, but no hop could be located, so there is no path to draw.");
  }
}
