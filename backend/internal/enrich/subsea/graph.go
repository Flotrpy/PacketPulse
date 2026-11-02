package subsea

import (
	"container/heap"
	"math"

	"github.com/flotrpy/packetpulse/backend/internal/enrich/gis"
)

// cableGraph turns a cable's MultiLineString into a routable graph. Vertices
// closer than snapKM (branching units, segment joins) are merged so traffic
// can move between the trunk and its branches.
type cableGraph struct {
	pts  []gis.LatLon
	adj  [][]edge
	node [][]int // node index for Lines[l][i]
}

type edge struct {
	to int
	km float64
}

const snapKM = 2.0

func (c *Cable) routing() *cableGraph {
	c.graphOnce.Do(func() { c.graph = buildGraph(c.Lines) })
	return c.graph
}

func buildGraph(lines [][]gis.LatLon) *cableGraph {
	g := &cableGraph{node: make([][]int, len(lines))}
	// Bucket by ~0.05 deg to find snap candidates quickly.
	buckets := map[[2]int][]int{}
	key := func(p gis.LatLon) [2]int { return [2]int{int(math.Floor(p.Lat * 20)), int(math.Floor(p.Lon * 20))} }
	find := func(p gis.LatLon) int {
		// TeleGeography splits trans-Pacific lines at the antimeridian; a
		// point at lon +180 must snap to its twin at -180.
		probes := []gis.LatLon{p}
		if math.Abs(p.Lon) > 179.9 {
			probes = append(probes, gis.LatLon{Lat: p.Lat, Lon: p.Lon - math.Copysign(360, p.Lon)})
		}
		for _, q := range probes {
			k := key(q)
			for dl := -1; dl <= 1; dl++ {
				for dn := -1; dn <= 1; dn++ {
					for _, id := range buckets[[2]int{k[0] + dl, k[1] + dn}] {
						if gis.HaversineKM(p, g.pts[id]) <= snapKM {
							return id
						}
					}
				}
			}
		}
		id := len(g.pts)
		g.pts = append(g.pts, p)
		g.adj = append(g.adj, nil)
		buckets[key(p)] = append(buckets[key(p)], id)
		return id
	}
	for li, line := range lines {
		g.node[li] = make([]int, len(line))
		for i, p := range line {
			g.node[li][i] = find(p)
			if i > 0 {
				a, b := g.node[li][i-1], g.node[li][i]
				if a != b {
					d := gis.HaversineKM(line[i-1], p)
					g.adj[a] = append(g.adj[a], edge{b, d})
					g.adj[b] = append(g.adj[b], edge{a, d})
				}
			}
		}
	}
	return g
}

// Route is the along-cable path between two projections.
type Route struct {
	KM   float64
	Path []gis.LatLon
}

// RouteBetween returns the shortest path along the cable between two
// projected points, or ok=false when they lie on disconnected parts.
func (c *Cable) RouteBetween(a, b Projection) (Route, bool) {
	g := c.routing()
	// Same segment: straight along it.
	if a.Line == b.Line && a.Seg == b.Seg {
		return Route{KM: gis.HaversineKM(a.Point, b.Point), Path: []gis.LatLon{a.Point, b.Point}}, true
	}
	segEnds := func(p Projection) (int, float64, int, float64) {
		line := c.Lines[p.Line]
		u, v := g.node[p.Line][p.Seg], g.node[p.Line][p.Seg+1]
		return u, gis.HaversineKM(p.Point, line[p.Seg]), v, gis.HaversineKM(p.Point, line[p.Seg+1])
	}
	su, sdu, sv, sdv := segEnds(a)
	tu, tdu, tv, tdv := segEnds(b)

	dist := make([]float64, len(g.pts))
	prev := make([]int, len(g.pts))
	for i := range dist {
		dist[i], prev[i] = math.Inf(1), -1
	}
	pq := &minHeap{}
	push := func(n int, d float64) {
		if d < dist[n] {
			dist[n] = d
			heap.Push(pq, item{n, d})
		}
	}
	push(su, sdu)
	push(sv, sdv)
	for pq.Len() > 0 {
		it := heap.Pop(pq).(item)
		if it.d > dist[it.n] {
			continue
		}
		for _, e := range g.adj[it.n] {
			if nd := it.d + e.km; nd < dist[e.to] {
				dist[e.to] = nd
				prev[e.to] = it.n
				heap.Push(pq, item{e.to, nd})
			}
		}
	}
	end, total := tu, dist[tu]+tdu
	if dist[tv]+tdv < total {
		end, total = tv, dist[tv]+tdv
	}
	if math.IsInf(total, 1) {
		return Route{}, false
	}
	var rev []gis.LatLon
	for n := end; n != -1; n = prev[n] {
		rev = append(rev, g.pts[n])
	}
	path := make([]gis.LatLon, 0, len(rev)+2)
	path = append(path, a.Point)
	for i := len(rev) - 1; i >= 0; i-- {
		path = append(path, rev[i])
	}
	path = append(path, b.Point)
	return Route{KM: total, Path: path}, true
}

type item struct {
	n int
	d float64
}
type minHeap []item

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].d < h[j].d }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(item)) }
func (h *minHeap) Pop() any          { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }
