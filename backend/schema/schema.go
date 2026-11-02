// Package schema embeds the PacketJourney JSON Schema so the service can
// serve it (GET /v1/schema) and validate documents against it.
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

const schemaURL = "https://packetpulse.dev/schemas/ingest/packet-journey.schema.json"

var (
	once     sync.Once
	compiled *jsonschema.Schema
	compErr  error
)

func compile() (*jsonschema.Schema, error) {
	once.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(PacketJourney))
		if err != nil {
			compErr = err
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		if err := c.AddResource(schemaURL, doc); err != nil {
			compErr = err
			return
		}
		compiled, compErr = c.Compile(schemaURL)
	})
	return compiled, compErr
}

// Validate marshals v (e.g. a *model.Journey) and checks it against the schema.
func Validate(v any) error {
	s, err := compile()
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
