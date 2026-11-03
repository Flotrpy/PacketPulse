/**
 * The Earth: NASA-derived imagery (day, night lights, clouds, ocean mask)
 * lit by the real sun for the current UTC time, with country borders as one
 * LineSegments draw call, an atmosphere rim and a star field. Until the
 * textures arrive (or if they fail) a stylised land/ocean sphere rasterised
 * from Natural Earth polygons (public/data/land.json) is shown.
 */
import * as THREE from "three";
import { latLonToVec3 } from "../core/geo";

export interface LandData {
  scale: number;
  countries: { iso2: string; name: string; rings: number[][] }[];
}

/** Texture files under the textures base URL. */
export const EARTH_TEXTURES = {
  day: "earth_day_4k.jpg",
  night: "earth_night_4k.jpg",
  clouds: "earth_clouds_2k.jpg",
  specular: "earth_specular_2k.jpg",
};

/**
 * Unit vector towards the sun in globe coordinates (same frame as
 * latLonToVec3): subsolar latitude = solar declination, subsolar longitude
 * from UTC time and the equation of time. Accurate to well under a degree,
 * plenty for a day/night terminator.
 */
export function sunDirection(date: Date): [number, number, number] {
  const start = Date.UTC(date.getUTCFullYear(), 0, 0);
  const day = (date.getTime() - start) / 86400000; // fractional day of year
  const decl = 23.44 * Math.sin((2 * Math.PI * (284 + day)) / 365);
  const b = (2 * Math.PI * (day - 81)) / 364;
  const eotMin = 9.87 * Math.sin(2 * b) - 7.53 * Math.cos(b) - 1.5 * Math.sin(b);
  const utcHours = date.getUTCHours() + date.getUTCMinutes() / 60 + date.getUTCSeconds() / 3600;
  let lon = -15 * (utcHours - 12) - eotMin / 4;
  lon = ((lon + 540) % 360) - 180;
  return latLonToVec3(decl, lon, 1);
}

const earthVertex = /* glsl */ `
varying vec3 vObj;
varying vec3 vWorld;
varying vec3 vNormalV;
varying vec3 vViewDir;
void main() {
  vObj = position;
  vec4 world = modelMatrix * vec4(position, 1.0);
  vWorld = world.xyz;
  vec4 mv = viewMatrix * world;
  vNormalV = normalize(normalMatrix * normal);
  vViewDir = normalize(-mv.xyz);
  gl_Position = projectionMatrix * mv;
}
`;

const earthFragment = /* glsl */ `
uniform sampler2D uLand;
uniform float uHasLand;
uniform sampler2D uRegion;   // r = permitted territory, g = prohibited territory
uniform float uHasRegion;
uniform float uBusiness;
uniform sampler2D uDay;
uniform sampler2D uNight;
uniform sampler2D uSpec;     // 1 = water
uniform float uHasTex;
uniform vec3 uSun;           // unit vector towards the sun (globe frame)
varying vec3 vObj;
varying vec3 vWorld;
varying vec3 vNormalV;
varying vec3 vViewDir;
const float PI = 3.141592653589793;
void main() {
  vec3 n = normalize(vObj);
  float lat = asin(clamp(n.y, -1.0, 1.0));
  float lon = atan(n.x, n.z);
  vec2 uv = vec2((lon + PI) / (2.0 * PI), (lat + PI * 0.5) / PI);
  float land = uHasLand * texture2D(uLand, uv).r;
  float facing = clamp(dot(normalize(vNormalV), normalize(vViewDir)), 0.0, 1.0);
  vec3 col;

  if (uHasTex > 0.5) {
    // ---- realistic: sunlit day side, city lights at night ----
    float ndl = dot(n, uSun);
    float dayAmt = smoothstep(-0.12, 0.22, ndl);
    vec3 day = texture2D(uDay, uv).rgb;
    vec3 night = texture2D(uNight, uv).rgb;
    float water = texture2D(uSpec, uv).r;
    float diffuse = clamp(ndl, 0.0, 1.0);
    vec3 lit = day * (0.06 + 1.05 * pow(diffuse, 0.75));
    // Ocean glint: Blinn-Phong on water only.
    vec3 v = normalize(cameraPosition - vWorld);
    vec3 h = normalize(uSun + v);
    float spec = pow(max(dot(n, h), 0.0), 70.0) * water * dayAmt;
    lit += vec3(1.0, 0.92, 0.78) * spec * 0.55;
    // Night: faint moonlit surface plus city lights, warm-tinted.
    vec3 dark = day * 0.05 + pow(night, vec3(1.6)) * vec3(1.35, 1.1, 0.8) * 1.6;
    col = mix(dark, lit, dayAmt);
    // Twilight band.
    float dusk = smoothstep(-0.12, 0.0, ndl) * (1.0 - smoothstep(0.0, 0.18, ndl));
    col += vec3(0.35, 0.12, 0.04) * dusk * 0.35;
    if (uBusiness > 0.5) {
      // Desaturate so the jurisdiction tints read clearly.
      float l = dot(col, vec3(0.299, 0.587, 0.114));
      col = mix(col, vec3(l) * vec3(0.75, 0.85, 1.0), 0.65);
    }
  } else {
    // ---- stylised fallback: dot-matrix land ----
    float rows = 180.0;
    float row = floor(uv.y * rows);
    float rowLat = ((row + 0.5) / rows - 0.5) * PI;
    float cols = max(1.0, floor(360.0 * cos(rowLat)));
    vec2 cell = vec2(fract(uv.x * cols), fract(uv.y * rows)) - 0.5;
    float r = length(cell);
    float aa = max(fwidth(r), 1e-4);
    float dots = 1.0 - smoothstep(0.2 - aa, 0.2 + aa, r);
    float light = 0.35 + 0.65 * pow(facing, 0.6);
    vec3 ocean = mix(vec3(0.010, 0.025, 0.060), vec3(0.020, 0.060, 0.120), facing);
    col = mix(ocean, vec3(0.050, 0.090, 0.140) + vec3(0.180, 0.420, 0.620) * dots, land) * light;
  }

  // Business Mode: tint permitted jurisdictions teal and prohibited ones red;
  // land outside both is dimmed so the regulatory boundary reads at a glance.
  if (uBusiness > 0.5) {
    vec2 rg = uHasRegion * texture2D(uRegion, uv).rg;
    float inside = max(rg.r, rg.g);
    col = mix(col, col * 0.45, land * (1.0 - inside));
    col += (vec3(0.06, 0.55, 0.62) * rg.r + vec3(0.75, 0.08, 0.08) * rg.g) * (0.25 + 0.45 * land);
  }
  // Atmospheric haze towards the limb.
  col += vec3(0.10, 0.35, 0.70) * pow(1.0 - facing, 3.0) * (uHasTex > 0.5 ? 0.35 : 0.6);
  gl_FragColor = vec4(col, 1.0);
}
`;

const cloudVertex = /* glsl */ `
varying vec3 vObj;
varying vec3 vNormalV;
varying vec3 vViewDir;
void main() {
  vObj = position;
  vec4 mv = modelViewMatrix * vec4(position, 1.0);
  vNormalV = normalize(normalMatrix * normal);
  vViewDir = normalize(-mv.xyz);
  gl_Position = projectionMatrix * mv;
}
`;

const cloudFragment = /* glsl */ `
uniform sampler2D uClouds;
uniform vec3 uSun;
uniform float uDrift;     // longitude offset (radians)
uniform float uOpacity;
varying vec3 vObj;
varying vec3 vNormalV;
varying vec3 vViewDir;
const float PI = 3.141592653589793;
void main() {
  vec3 n = normalize(vObj);
  float lat = asin(clamp(n.y, -1.0, 1.0));
  float lon = atan(n.x, n.z) + uDrift;
  vec2 uv = vec2(fract((lon + PI) / (2.0 * PI)), (lat + PI * 0.5) / PI);
  float c = texture2D(uClouds, uv).r;
  float a = smoothstep(0.12, 0.85, c);
  float ndl = dot(n, uSun);
  float lit = 0.04 + 0.96 * smoothstep(-0.15, 0.35, ndl);
  float facing = clamp(dot(normalize(vNormalV), normalize(vViewDir)), 0.0, 1.0);
  gl_FragColor = vec4(vec3(lit), a * uOpacity * (0.35 + 0.65 * facing));
}
`;

const atmoVertex = /* glsl */ `
varying vec3 vNormalV;
varying vec3 vViewDir;
varying vec3 vObj;
void main() {
  vObj = position;
  vec4 mv = modelViewMatrix * vec4(position, 1.0);
  vNormalV = normalize(normalMatrix * normal);
  vViewDir = normalize(-mv.xyz);
  gl_Position = projectionMatrix * mv;
}
`;

const atmoFragment = /* glsl */ `
uniform vec3 uSun;
uniform float uLit;
varying vec3 vNormalV;
varying vec3 vViewDir;
varying vec3 vObj;
void main() {
  float f = 1.0 - abs(dot(normalize(vNormalV), normalize(vViewDir)));
  float a = pow(f, 4.0);
  // Brighter where the sun lights the atmosphere (only with real lighting).
  float sun = mix(1.0, 0.25 + 1.1 * smoothstep(-0.35, 0.4, dot(normalize(vObj), uSun)), uLit);
  gl_FragColor = vec4(vec3(0.25, 0.6, 1.0) * a * 1.4 * sun, a * sun);
}
`;

export class Earth {
  readonly group = new THREE.Group();
  private material: THREE.ShaderMaterial;
  private cloudMaterial: THREE.ShaderMaterial;
  private atmoMaterial: THREE.ShaderMaterial;
  private clouds: THREE.Mesh;
  private textures: THREE.Texture[] = [];
  private sun = new THREE.Vector3(0, 0, 1);
  private lastSun = -Infinity;
  private business = false;
  private landTexture?: THREE.CanvasTexture;
  private borders?: THREE.LineSegments;
  private regionTexture?: THREE.CanvasTexture;
  private regionLines?: THREE.LineSegments;
  private disposables: { dispose(): void }[] = [];

  constructor() {
    const sphere = new THREE.SphereGeometry(1, 160, 120);
    this.material = new THREE.ShaderMaterial({
      vertexShader: earthVertex,
      fragmentShader: earthFragment,
      uniforms: {
        uLand: { value: null }, uHasLand: { value: 0 }, uRegion: { value: null }, uHasRegion: { value: 0 }, uBusiness: { value: 0 },
        uDay: { value: null }, uNight: { value: null }, uSpec: { value: null }, uHasTex: { value: 0 }, uSun: { value: this.sun },
      },
    });
    const earth = new THREE.Mesh(sphere, this.material);
    earth.renderOrder = 0;
    this.group.add(earth);

    // Clouds just above the surface. No depth writes, so arcs and hop
    // markers drawn afterwards are never hidden behind them.
    const cloudGeo = new THREE.SphereGeometry(1.005, 128, 96);
    this.cloudMaterial = new THREE.ShaderMaterial({
      vertexShader: cloudVertex,
      fragmentShader: cloudFragment,
      uniforms: { uClouds: { value: null }, uSun: { value: this.sun }, uDrift: { value: 0 }, uOpacity: { value: 0 } },
      transparent: true,
      depthWrite: false,
    });
    this.clouds = new THREE.Mesh(cloudGeo, this.cloudMaterial);
    this.clouds.renderOrder = 0.5;
    this.clouds.visible = false;
    this.group.add(this.clouds);

    const atmoGeo = new THREE.SphereGeometry(1.06, 96, 64);
    const atmoMat = (this.atmoMaterial = new THREE.ShaderMaterial({
      vertexShader: atmoVertex,
      fragmentShader: atmoFragment,
      uniforms: { uSun: { value: this.sun }, uLit: { value: 0 } },
      side: THREE.BackSide,
      transparent: true,
      depthWrite: false,
      blending: THREE.AdditiveBlending,
    }));
    const atmo = new THREE.Mesh(atmoGeo, atmoMat);
    atmo.renderOrder = 1;
    this.group.add(atmo);

    const stars = this.makeStars(2500);
    this.group.add(stars);
    this.disposables.push(sphere, this.material, cloudGeo, this.cloudMaterial, atmoGeo, atmoMat, stars.geometry, stars.material as THREE.Material);
    this.updateSun(new Date());
  }

  /** Loads the realistic textures; the stylised sphere stays until they arrive. */
  async loadTextures(baseUrl: string, renderer?: THREE.WebGLRenderer): Promise<void> {
    const loader = new THREE.TextureLoader();
    const base = baseUrl.endsWith("/") ? baseUrl : baseUrl + "/";
    // Sampled as raw sRGB values, like every other colour in the globe's shaders.
    const load = async (name: string) => {
      const t = await loader.loadAsync(base + name);
      t.colorSpace = THREE.NoColorSpace;
      t.anisotropy = Math.min(8, renderer?.capabilities.getMaxAnisotropy() ?? 4);
      t.wrapS = THREE.RepeatWrapping;
      this.textures.push(t);
      return t;
    };
    const [day, night, spec] = await Promise.all([
      load(EARTH_TEXTURES.day),
      load(EARTH_TEXTURES.night),
      load(EARTH_TEXTURES.specular),
    ]);
    const u = this.material.uniforms;
    u.uDay.value = day;
    u.uNight.value = night;
    u.uSpec.value = spec;
    u.uHasTex.value = 1;
    this.atmoMaterial.uniforms.uLit.value = 1;
    if (this.borders) (this.borders.material as THREE.LineBasicMaterial).setValues({ color: 0xc8dcff, opacity: 0.22 });
    // Clouds are optional polish: load them last.
    load(EARTH_TEXTURES.clouds)
      .then((clouds) => {
        this.cloudMaterial.uniforms.uClouds.value = clouds;
        this.clouds.visible = true;
        this.cloudMaterial.uniforms.uOpacity.value = this.business ? 0.15 : 0.85;
      })
      .catch((err) => console.warn("Earth: clouds not loaded", err));
  }

  /** Per frame: cloud drift and (once a minute) the sun position. */
  update(timeSec: number): void {
    this.cloudMaterial.uniforms.uDrift.value = (timeSec * 0.004) % (Math.PI * 2);
    if (timeSec - this.lastSun > 60) {
      this.lastSun = timeSec;
      this.updateSun(new Date());
    }
  }

  private updateSun(d: Date): void {
    const [x, y, z] = sunDirection(d);
    this.sun.set(x, y, z);
  }

  async loadLand(url: string): Promise<void> {
    const res = await fetch(url);
    if (!res.ok) throw new Error(`land data ${url}: HTTP ${res.status}`);
    this.setLand((await res.json()) as LandData);
  }

  setLand(data: LandData, textureWidth = 4096): void {
    const w = textureWidth;
    const h = textureWidth / 2;
    const canvas = document.createElement("canvas");
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext("2d");
    if (!ctx) throw new Error("2D canvas unavailable");
    ctx.fillStyle = "#000";
    ctx.fillRect(0, 0, w, h);
    ctx.fillStyle = "#fff";
    const s = data.scale;
    const borderPts: number[] = [];
    const v = [0, 0, 0] as [number, number, number];
    for (const c of data.countries) {
      for (const ring of c.rings) {
        ctx.beginPath();
        let prevLon = ring[0] / s;
        for (let i = 0; i < ring.length; i += 2) {
          const lon = ring[i] / s;
          const lat = ring[i + 1] / s;
          // Equirectangular: x from lon, y from lat (north up).
          const x = ((lon + 180) / 360) * w;
          const y = ((90 - lat) / 180) * h;
          if (i === 0 || Math.abs(lon - prevLon) > 180) ctx.moveTo(x, y);
          else ctx.lineTo(x, y);
          if (i > 0 && Math.abs(lon - prevLon) <= 180) {
            latLonToVec3(ring[i - 1] / s, prevLon, 1.0008, v);
            borderPts.push(v[0], v[1], v[2]);
            latLonToVec3(lat, lon, 1.0008, v);
            borderPts.push(v[0], v[1], v[2]);
          }
          prevLon = lon;
        }
        ctx.closePath();
        ctx.fill();
      }
    }
    this.landTexture?.dispose();
    this.landTexture = new THREE.CanvasTexture(canvas);
    this.landTexture.colorSpace = THREE.NoColorSpace;
    this.landTexture.anisotropy = 4;
    this.material.uniforms.uLand.value = this.landTexture;
    this.material.uniforms.uHasLand.value = 1;

    if (this.borders) {
      this.group.remove(this.borders);
      this.borders.geometry.dispose();
    }
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(borderPts, 3));
    const realistic = this.material.uniforms.uHasTex.value > 0.5;
    const m = new THREE.LineBasicMaterial({ color: realistic ? 0xc8dcff : 0x3f7fb5, transparent: true, opacity: realistic ? 0.22 : 0.35, depthWrite: false });
    this.borders = new THREE.LineSegments(g, m);
    this.borders.renderOrder = 1;
    this.group.add(this.borders);
  }

  setBusinessMode(on: boolean): void {
    this.business = on;
    this.material.uniforms.uBusiness.value = on ? 1 : 0;
    this.cloudMaterial.uniforms.uOpacity.value = on ? 0.15 : 0.85;
    if (this.regionLines) this.regionLines.visible = on;
  }

  /**
   * Draws regulatory boundaries from the compliance engine's region
   * geometry (GeoJSON FeatureCollections of Polygon/MultiPolygon features):
   * a tint texture sampled by the Earth shader plus border lines.
   */
  setRegionOverlay(permitted: GeoJSONFeatureCollection[], prohibited: GeoJSONFeatureCollection[], textureWidth = 2048): void {
    const w = textureWidth;
    const h = textureWidth / 2;
    const canvas = document.createElement("canvas");
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext("2d");
    if (!ctx) throw new Error("2D canvas unavailable");
    ctx.fillStyle = "#000";
    ctx.fillRect(0, 0, w, h);
    ctx.globalCompositeOperation = "lighter"; // permitted (red channel) and prohibited (green) add up
    const pos: number[] = [];
    const col: number[] = [];
    const v = [0, 0, 0] as [number, number, number];
    const draw = (fcs: GeoJSONFeatureCollection[], fill: string, rgb: [number, number, number]) => {
      ctx.fillStyle = fill;
      for (const fc of fcs) {
        for (const feat of fc.features) {
          for (const poly of polygonsOf(feat.geometry)) {
            ctx.beginPath();
            for (const ring of poly) {
              ring.forEach(([lon, lat], i) => {
                const x = ((lon + 180) / 360) * w;
                const y = ((90 - lat) / 180) * h;
                if (i === 0) ctx.moveTo(x, y);
                else ctx.lineTo(x, y);
                if (i > 0) {
                  const [plon, plat] = ring[i - 1];
                  if (Math.abs(lon - plon) > 180) return; // antimeridian seam
                  latLonToVec3(plat, plon, 1.0012, v);
                  pos.push(...v);
                  latLonToVec3(lat, lon, 1.0012, v);
                  pos.push(...v);
                  col.push(...rgb, ...rgb);
                }
              });
              ctx.closePath();
            }
            ctx.fill("evenodd");
          }
        }
      }
    };
    draw(permitted, "#f00", [0.2, 0.85, 0.95]);
    draw(prohibited, "#0f0", [1.0, 0.25, 0.25]);

    this.regionTexture?.dispose();
    this.regionTexture = new THREE.CanvasTexture(canvas);
    this.regionTexture.colorSpace = THREE.NoColorSpace;
    this.material.uniforms.uRegion.value = this.regionTexture;
    this.material.uniforms.uHasRegion.value = permitted.length + prohibited.length > 0 ? 1 : 0;

    if (this.regionLines) {
      this.group.remove(this.regionLines);
      this.regionLines.geometry.dispose();
      (this.regionLines.material as THREE.Material).dispose();
    }
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.Float32BufferAttribute(pos, 3));
    g.setAttribute("color", new THREE.Float32BufferAttribute(col, 3));
    this.regionLines = new THREE.LineSegments(g, new THREE.LineBasicMaterial({ vertexColors: true, transparent: true, opacity: 0.85, depthWrite: false }));
    this.regionLines.renderOrder = 1;
    this.regionLines.visible = this.material.uniforms.uBusiness.value > 0.5;
    this.group.add(this.regionLines);
  }

  private makeStars(n: number): THREE.Points {
    const pos = new Float32Array(n * 3);
    let seed = 1337;
    const rnd = () => ((seed = (seed * 1664525 + 1013904223) >>> 0) / 4294967296);
    for (let i = 0; i < n; i++) {
      const u = rnd() * 2 - 1;
      const th = rnd() * Math.PI * 2;
      const r = 60 + rnd() * 30;
      const q = Math.sqrt(1 - u * u);
      pos.set([r * q * Math.cos(th), r * u, r * q * Math.sin(th)], i * 3);
    }
    const g = new THREE.BufferGeometry();
    g.setAttribute("position", new THREE.BufferAttribute(pos, 3));
    return new THREE.Points(g, new THREE.PointsMaterial({ color: 0x8fb4ff, size: 0.12, sizeAttenuation: true, transparent: true, opacity: 0.55, depthWrite: false }));
  }

  dispose(): void {
    this.textures.forEach((t) => t.dispose());
    this.landTexture?.dispose();
    this.regionTexture?.dispose();
    this.regionLines?.geometry.dispose();
    this.borders?.geometry.dispose();
    (this.borders?.material as THREE.Material | undefined)?.dispose();
    for (const d of this.disposables) d.dispose();
  }
}

export interface GeoJSONGeometry {
  type: string;
  coordinates: unknown;
}
export interface GeoJSONFeatureCollection {
  type: "FeatureCollection";
  features: { type: "Feature"; properties?: Record<string, unknown>; geometry: GeoJSONGeometry }[];
}

/** Polygons (each a list of [lon, lat] rings) of a Polygon or MultiPolygon. */
export function polygonsOf(g: GeoJSONGeometry): [number, number][][][] {
  if (g.type === "Polygon") return [g.coordinates as [number, number][][]];
  if (g.type === "MultiPolygon") return g.coordinates as [number, number][][][];
  return [];
}
