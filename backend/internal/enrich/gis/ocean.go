package gis

import "math"

// CrossingOptions tunes ocean detection.
type CrossingOptions struct {
	// StepKM is the great-circle sampling interval.
	StepKM float64
	// MinWaterRunKM: a contiguous stretch of water at least this long counts
	// as an ocean crossing. Shorter runs are coastline wobble (cities sit on
	// coasts, and the 1:50m coastline is generalised) or rivers/estuaries.
	MinWaterRunKM float64
	// CoastTrimKM ignores water within this distance of each endpoint, so a
	// hop placed in a harbour city does not start "at sea".
	CoastTrimKM float64
	// CoastalOffshoreKM: a same-country run that never gets farther than this
	// from land is coastal (see WaterRun.Coastal).
	CoastalOffshoreKM float64
}

func (o CrossingOptions) withDefaults() CrossingOptions {
	if o.StepKM <= 0 {
		o.StepKM = 10
	}
	if o.MinWaterRunKM <= 0 {
		o.MinWaterRunKM = 30
	}
	if o.CoastalOffshoreKM <= 0 {
		o.CoastalOffshoreKM = 100
	}
	if o.CoastTrimKM < 0 {
		o.CoastTrimKM = 0
	} else if o.CoastTrimKM == 0 {
		o.CoastTrimKM = 15
	}
	return o
}

// WaterRun is one contiguous stretch of water along the path.
type WaterRun struct {
	FromKM   float64 `json:"from_km"`
	ToKM     float64 `json:"to_km"`
	LengthKM float64 `json:"length_km"`
	Entry    LatLon  `json:"entry"` // last land point before the water
	Exit     LatLon  `json:"exit"`  // first land point after the water
	// MaxOffshoreKM: how far the run gets from the nearest land (probed up to 200 km).
	MaxOffshoreKM float64 `json:"max_offshore_km"`
	// Coastal: the run leaves and rejoins the same country without going far
	// offshore (the great circle clipping a bay or a coastline). Traffic
	// follows the coast overland, so it is not an ocean crossing.
	Coastal bool `json:"coastal"`

	entryCC string
}

// Crossing describes the great-circle path between two hops.
type Crossing struct {
	DistanceKM     float64    `json:"distance_km"`
	CrossesOcean   bool       `json:"crosses_ocean"`
	WaterKM        float64    `json:"water_km"`
	LongestWaterKM float64    `json:"longest_water_km"`
	WaterFraction  float64    `json:"water_fraction"`
	Countries      []string   `json:"countries_overflown"` // land countries along the path, in order
	WaterRuns      []WaterRun `json:"water_runs"`
	// WaterSamples are points on the water runs, used to compare the path
	// with cable geometry.
	WaterSamples []LatLon `json:"-"`
}

// AnalyzeCrossing samples the great circle from a to b and classifies each
// sample as land (with its country) or water.
func (idx *LandIndex) AnalyzeCrossing(a, b LatLon, opts CrossingOptions) Crossing {
	o := opts.withDefaults()
	dist := HaversineKM(a, b)
	c := Crossing{DistanceKM: dist, Countries: []string{}, WaterRuns: []WaterRun{}}
	n := int(math.Ceil(dist / o.StepKM))
	if n < 2 {
		n = 2
	}
	if n > 4000 {
		n = 4000
	}
	step := dist / float64(n)

	var run *WaterRun
	var runSamples []LatLon
	lastLand, lastCC := a, ""
	addCountry := func(cc string) {
		if cc != "" && (len(c.Countries) == 0 || c.Countries[len(c.Countries)-1] != cc) {
			c.Countries = append(c.Countries, cc)
		}
	}
	closeRun := func(exit LatLon, exitCC string, atKM float64) {
		if run == nil {
			return
		}
		run.ToKM = atKM
		run.LengthKM = run.ToKM - run.FromKM
		run.Exit = exit
		if run.LengthKM >= o.MinWaterRunKM {
			run.MaxOffshoreKM = idx.maxOffshore(runSamples)
			run.Coastal = exitCC != "" && exitCC == run.entryCC && run.MaxOffshoreKM < o.CoastalOffshoreKM
			c.WaterRuns = append(c.WaterRuns, *run)
			if !run.Coastal {
				c.WaterKM += run.LengthKM
				c.LongestWaterKM = math.Max(c.LongestWaterKM, run.LengthKM)
				c.WaterSamples = append(c.WaterSamples, runSamples...)
			}
		}
		run, runSamples = nil, nil
	}

	for i := 0; i <= n; i++ {
		km := float64(i) * step
		p := Interpolate(a, b, float64(i)/float64(n))
		cc, land := idx.CountryAt(p)
		nearEnd := km < o.CoastTrimKM || dist-km < o.CoastTrimKM
		if land || nearEnd {
			addCountry(cc)
			if !land {
				cc, _ = idx.nearestCountry(p)
			}
			closeRun(p, cc, km)
			lastLand, lastCC = p, cc
			continue
		}
		if run == nil {
			run = &WaterRun{FromKM: math.Max(0, km-step), Entry: lastLand, entryCC: lastCC}
		}
		runSamples = append(runSamples, p)
	}
	closeRun(b, "", dist)

	c.CrossesOcean = c.LongestWaterKM > 0
	if dist > 0 {
		c.WaterFraction = math.Round(c.WaterKM/dist*1000) / 1000
	}
	c.DistanceKM = math.Round(c.DistanceKM*10) / 10
	c.WaterKM = math.Round(c.WaterKM*10) / 10
	c.LongestWaterKM = math.Round(c.LongestWaterKM*10) / 10
	return c
}

var probeRadiiKM = []float64{10, 25, 50, 75, 100, 150, 200}

// offshore returns the distance (km, from probeRadiiKM) at which land is
// found around p, probing 8 bearings; 200+ when none within 200 km.
func (idx *LandIndex) offshore(p LatLon) (float64, string) {
	for _, r := range probeRadiiKM {
		for b := 0; b < 8; b++ {
			q := Destination(p, float64(b)*45, r)
			if cc, ok := idx.CountryAt(q); ok {
				return r, cc
			}
		}
	}
	return 200, ""
}

func (idx *LandIndex) nearestCountry(p LatLon) (string, bool) {
	if cc, ok := idx.CountryAt(p); ok {
		return cc, true
	}
	_, cc := idx.offshore(p)
	return cc, cc != ""
}

// maxOffshore probes up to 24 samples of a run.
func (idx *LandIndex) maxOffshore(samples []LatLon) float64 {
	step := 1
	if len(samples) > 24 {
		step = len(samples) / 24
	}
	best := 0.0
	for i := 0; i < len(samples); i += step {
		d, _ := idx.offshore(samples[i])
		best = math.Max(best, d)
	}
	return best
}
