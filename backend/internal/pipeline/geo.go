package pipeline

import (
	"math"
	"net/netip"
)

// Light travels about 204 km per millisecond in optical fiber (c / 1.468).
const fiberKMPerMS = 204.2

func haversineKM(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dp, dl := (lat2-lat1)*math.Pi/180, (lon2-lon1)*math.Pi/180
	a := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(a)))
}

// minFeasibleRTT is the round-trip time light needs over distanceKM of fiber.
func minFeasibleRTT(distanceKM float64) float64 { return 2 * distanceKM / fiberKMPerMS }

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round1(v float64) float64 { return math.Round(v*10) / 10 }

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// reserved holds special-purpose ranges (RFC 5737 documentation, RFC 2544
// benchmarking, 0/8, 240/4, IPv6 documentation) that never geolocate.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"),
}

func addressClass(a netip.Addr) string {
	switch {
	case !a.IsValid():
		return "unknown"
	case a.IsLoopback():
		return "loopback"
	case a.IsLinkLocalUnicast():
		return "link_local"
	case a.IsPrivate():
		return "private"
	case a.Is4() && cgnat.Contains(a):
		return "cgnat"
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return "reserved"
		}
	}
	return "public"
}

// landConnected lists continent pairs joined by land, where long-haul fiber
// can run overland (Natural Earth continent names).
var landConnected = map[[2]string]bool{
	{"Asia", "Europe"}: true, {"Africa", "Asia"}: true, {"North America", "South America"}: true,
}

func continentsLandConnected(a, b string) bool {
	if a == b {
		return true
	}
	if b < a {
		a, b = b, a
	}
	return landConnected[[2]string{a, b}]
}
