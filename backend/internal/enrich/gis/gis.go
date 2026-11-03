// Package gis holds the spherical geometry used by the enrichment layer:
// great-circle maths, a gridded land/country index built from Natural Earth
// polygons, and ocean-crossing analysis between two hop coordinates.
package gis

import "math"

const EarthRadiusKM = 6371.0

// LatLon is a WGS84 coordinate in degrees.
type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

func rad(d float64) float64 { return d * math.Pi / 180 }
func deg(r float64) float64 { return r * 180 / math.Pi }

// HaversineKM is the great-circle distance in km.
func HaversineKM(a, b LatLon) float64 {
	p1, p2 := rad(a.Lat), rad(b.Lat)
	dp, dl := rad(b.Lat-a.Lat), rad(b.Lon-a.Lon)
	h := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * EarthRadiusKM * math.Asin(math.Min(1, math.Sqrt(h)))
}

type vec3 [3]float64

func toVec(p LatLon) vec3 {
	la, lo := rad(p.Lat), rad(p.Lon)
	return vec3{math.Cos(la) * math.Cos(lo), math.Cos(la) * math.Sin(lo), math.Sin(la)}
}

func toLatLon(v vec3) LatLon {
	return LatLon{Lat: deg(math.Atan2(v[2], math.Hypot(v[0], v[1]))), Lon: deg(math.Atan2(v[1], v[0]))}
}

// Interpolate returns the point a fraction f of the way along the great
// circle from a to b.
func Interpolate(a, b LatLon, f float64) LatLon {
	va, vb := toVec(a), toVec(b)
	d := math.Acos(math.Max(-1, math.Min(1, va[0]*vb[0]+va[1]*vb[1]+va[2]*vb[2])))
	if d < 1e-12 {
		return a
	}
	s := math.Sin(d)
	wa, wb := math.Sin((1-f)*d)/s, math.Sin(f*d)/s
	return toLatLon(vec3{wa*va[0] + wb*vb[0], wa*va[1] + wb*vb[1], wa*va[2] + wb*vb[2]})
}

// PointSegmentKM is the distance from p to the segment a-b (km), using a
// local equirectangular projection centred on p. Accurate to well under 1%
// for segments a few hundred km long, which is what cable geometries use.
// It also returns the fraction t along a-b of the closest point.
func PointSegmentKM(p, a, b LatLon) (float64, float64) {
	kx := math.Cos(rad(p.Lat)) * EarthRadiusKM * math.Pi / 180
	ky := EarthRadiusKM * math.Pi / 180
	ax, ay := wrapLon(a.Lon-p.Lon)*kx, (a.Lat-p.Lat)*ky
	bx, by := wrapLon(b.Lon-p.Lon)*kx, (b.Lat-p.Lat)*ky
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = math.Max(0, math.Min(1, -(ax*dx+ay*dy)/l2))
	}
	cx, cy := ax+t*dx, ay+t*dy
	return math.Hypot(cx, cy), t
}

func wrapLon(d float64) float64 {
	for d > 180 {
		d -= 360
	}
	for d < -180 {
		d += 360
	}
	return d
}

// FiberKMPerMS: light in fiber covers ~204.2 km per ms (c / 1.468).
const FiberKMPerMS = 204.2

// MinRTTms is the round trip light needs over km of fiber.
func MinRTTms(km float64) float64 { return 2 * km / FiberKMPerMS }

// Destination is the point reached from p travelling km along bearingDeg.
func Destination(p LatLon, bearingDeg, km float64) LatLon {
	d := km / EarthRadiusKM
	br := rad(bearingDeg)
	la1, lo1 := rad(p.Lat), rad(p.Lon)
	la2 := math.Asin(math.Sin(la1)*math.Cos(d) + math.Cos(la1)*math.Sin(d)*math.Cos(br))
	lo2 := lo1 + math.Atan2(math.Sin(br)*math.Sin(d)*math.Cos(la1), math.Cos(d)-math.Sin(la1)*math.Sin(la2))
	return LatLon{Lat: deg(la2), Lon: wrapLon(deg(lo2))}
}
