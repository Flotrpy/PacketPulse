/**
 * The digital Earth: a shaded sphere whose land/ocean mask is rasterised at
 * runtime from Natural Earth polygons (public/data/land.json), country borders
 * as one LineSegments draw call, an atmosphere rim, and a star field.
 * No external tiles or textures are fetched.
 */
import * as THREE from "three";
import { latLonToVec3 } from "../core/geo";

export interface LandData {
  scale: number;
  countries: { iso2: string; name: string; rings: number[][] }[];
}

const earthVertex = /* glsl */ `
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

const earthFragment = /* glsl */ `
uniform sampler2D uLand;
uniform float uHasLand;
varying vec3 vObj;
varying vec3 vNormalV;
varying vec3 vViewDir;
const float PI = 3.141592653589793;
void main() {
  vec3 n = normalize(vObj);
  float lat = asin(clamp(n.y, -1.0, 1.0));
  float lon = atan(n.x, n.z);
  vec2 uv = vec2((lon + PI) / (2.0 * PI), (lat + PI * 0.5) / PI);
  float land = uHasLand * texture2D(uLand, uv).r;

  // Dot-matrix land: one dot per ~1 degree. The column count is fixed per
  // row (from the row's centre latitude) so dots stay round instead of
  // shearing, and keeps the spacing even towards the poles.
  float rows = 180.0;
  float row = floor(uv.y * rows);
  float rowLat = ((row + 0.5) / rows - 0.5) * PI;
  float cols = max(1.0, floor(360.0 * cos(rowLat)));
  vec2 cell = vec2(fract(uv.x * cols), fract(uv.y * rows)) - 0.5;
  float r = length(cell);
  float aa = max(fwidth(r), 1e-4);
  float dots = 1.0 - smoothstep(0.2 - aa, 0.2 + aa, r);

  // Graticule every 30 degrees.
  vec2 deg = vec2(lon, lat) * (180.0 / PI) / 30.0;
  vec2 gd = abs(fract(deg + 0.5) - 0.5) / fwidth(deg);
  float grid = 1.0 - min(min(gd.x, gd.y), 1.0);

  float facing = clamp(dot(normalize(vNormalV), normalize(vViewDir)), 0.0, 1.0);
  float light = 0.35 + 0.65 * pow(facing, 0.6);

  vec3 ocean = mix(vec3(0.010, 0.025, 0.060), vec3(0.020, 0.060, 0.120), facing);
  vec3 landBase = vec3(0.050, 0.090, 0.140);
  vec3 landDots = vec3(0.180, 0.420, 0.620);
  vec3 col = mix(ocean, landBase + landDots * dots, land);
  col += vec3(0.05, 0.12, 0.20) * grid * 0.6;
  col *= light;
  // Rim light towards the limb.
  col += vec3(0.10, 0.35, 0.70) * pow(1.0 - facing, 3.0) * 0.6;
  gl_FragColor = vec4(col, 1.0);
}
`;

const atmoVertex = /* glsl */ `
varying vec3 vNormalV;
varying vec3 vViewDir;
void main() {
  vec4 mv = modelViewMatrix * vec4(position, 1.0);
  vNormalV = normalize(normalMatrix * normal);
  vViewDir = normalize(-mv.xyz);
  gl_Position = projectionMatrix * mv;
}
`;

const atmoFragment = /* glsl */ `
varying vec3 vNormalV;
varying vec3 vViewDir;
void main() {
  float f = 1.0 - abs(dot(normalize(vNormalV), normalize(vViewDir)));
  float a = pow(f, 4.0);
  gl_FragColor = vec4(vec3(0.25, 0.6, 1.0) * a * 1.4, a);
}
`;

export class Earth {
  readonly group = new THREE.Group();
  private material: THREE.ShaderMaterial;
  private landTexture?: THREE.CanvasTexture;
  private borders?: THREE.LineSegments;
  private disposables: { dispose(): void }[] = [];

  constructor() {
    const sphere = new THREE.SphereGeometry(1, 160, 120);
    this.material = new THREE.ShaderMaterial({
      vertexShader: earthVertex,
      fragmentShader: earthFragment,
      uniforms: { uLand: { value: null }, uHasLand: { value: 0 } },
    });
    const earth = new THREE.Mesh(sphere, this.material);
    earth.renderOrder = 0;
    this.group.add(earth);

    const atmoGeo = new THREE.SphereGeometry(1.06, 96, 64);
    const atmoMat = new THREE.ShaderMaterial({
      vertexShader: atmoVertex,
      fragmentShader: atmoFragment,
      side: THREE.BackSide,
      transparent: true,
      depthWrite: false,
      blending: THREE.AdditiveBlending,
    });
    const atmo = new THREE.Mesh(atmoGeo, atmoMat);
    atmo.renderOrder = 1;
    this.group.add(atmo);

    const stars = this.makeStars(2500);
    this.group.add(stars);
    this.disposables.push(sphere, this.material, atmoGeo, atmoMat, stars.geometry, stars.material as THREE.Material);
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
    const m = new THREE.LineBasicMaterial({ color: 0x3f7fb5, transparent: true, opacity: 0.35, depthWrite: false });
    this.borders = new THREE.LineSegments(g, m);
    this.borders.renderOrder = 1;
    this.group.add(this.borders);
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
    this.landTexture?.dispose();
    this.borders?.geometry.dispose();
    (this.borders?.material as THREE.Material | undefined)?.dispose();
    for (const d of this.disposables) d.dispose();
  }
}
