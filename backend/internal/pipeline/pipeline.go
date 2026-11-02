// Package pipeline turns a RawTrace into an enriched model.Journey through an
// ordered list of decoupled stages:
//
//	normalize -> resolve -> infer_infrastructure -> compliance -> persist
//
// Each stage reads and writes the shared State. Required stages abort the run
// on error; enrichment stages are best-effort and record their error in the
// journey's pipeline audit trail instead.
package pipeline

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/flotrpy/packetpulse/backend/internal/model"
	"github.com/flotrpy/packetpulse/backend/internal/traceroute"
)

// Request carries caller context that is not in the raw trace.
type Request struct {
	Target         string // as submitted (hostname or IP)
	PolicyID       string
	AgentID        string
	AccessType     string // wired | wifi | cellular | unknown
	SourceLocation *model.Location

	// Data classification context: explicit labels win over rules; App,
	// SNI and Port describe the flow the trace stands in for.
	Classifications []string
	App, SNI        string
	Port            int
}

// State is passed through every stage.
type State struct {
	Raw     *traceroute.RawTrace
	Req     Request
	Journey *model.Journey

	// hop-level working data, indexed like Journey.Hops
	hopAddr  []netip.Addr
	hopMinMS []float64
	cellular *model.CellularHaul
	gwHop    int              // hop number of the mobile packet gateway (0 = none)
	cables   map[int][]string // segment index -> landing countries of the selected cable
}

type Stage interface {
	Name() string
	Required() bool
	Process(ctx context.Context, s *State) error
}

type Pipeline struct {
	stages []Stage
}

func New(stages ...Stage) *Pipeline { return &Pipeline{stages: stages} }

// Run executes all stages in order and returns the journey.
func (p *Pipeline) Run(ctx context.Context, raw *traceroute.RawTrace, req Request) (*model.Journey, error) {
	if raw == nil {
		return nil, fmt.Errorf("nil trace")
	}
	s := &State{Raw: raw, Req: req, Journey: &model.Journey{SchemaVersion: model.SchemaVersion}, cables: map[int][]string{}}
	for _, st := range p.stages {
		if err := ctx.Err(); err != nil {
			return s.Journey, err
		}
		// The entry is added before the stage runs so a persisting stage
		// stores an audit trail that includes itself.
		s.Journey.Pipeline.Stages = append(s.Journey.Pipeline.Stages, model.StageRun{Name: st.Name()})
		run := &s.Journey.Pipeline.Stages[len(s.Journey.Pipeline.Stages)-1]
		t0 := time.Now()
		err := st.Process(ctx, s)
		run.DurationMS = ms(time.Since(t0))
		if err != nil {
			run.Error = err.Error()
		}
		if err != nil && st.Required() {
			return s.Journey, fmt.Errorf("stage %s: %w", st.Name(), err)
		}
	}
	return s.Journey, nil
}

func ms(d time.Duration) float64 { return round3(float64(d) / float64(time.Millisecond)) }
