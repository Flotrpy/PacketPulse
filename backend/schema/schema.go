// Package schema embeds the JSON Schemas so the service can serve them and
// validate documents against them.
package schema

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed packet_journey.schema.json
var PacketJourney []byte

//go:embed infra_map.schema.json
var InfraMap []byte

const (
	packetJourneyURL = "https://packetpulse.dev/schemas/ingest/packet-journey.schema.json"
	infraMapURL      = "https://packetpulse.dev/schemas/enrich/infra-map.schema.json"
)

type compiled struct {
	once sync.Once
	s    *jsonschema.Schema
	err  error
}

var schemas = map[string]*compiled{packetJourneyURL: {}, infraMapURL: {}}

func get(url string, src []byte) (*jsonschema.Schema, error) {
	c := schemas[url]
	c.once.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(src))
		if err != nil {
			c.err = err
			return
		}
		comp := jsonschema.NewCompiler()
		comp.AssertFormat()
		if err := comp.AddResource(url, doc); err != nil {
			c.err = err
			return
		}
		c.s, c.err = comp.Compile(url)
	})
	return c.s, c.err
}

func validate(url string, src []byte, v any) error {
	s, err := get(url, src)
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return err
	}
	return s.Validate(inst)
}

// Validate checks a PacketJourney (e.g. *model.Journey).
func Validate(v any) error { return validate(packetJourneyURL, PacketJourney, v) }

// ValidateInfraMap checks an enrichment InfraMap (e.g. *enrich.InfraMap).
func ValidateInfraMap(v any) error { return validate(infraMapURL, InfraMap, v) }
