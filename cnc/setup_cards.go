package cnc

// Setup-sheet inputs: the "gmw-nc-cards" JSON the Fusion post writes next
// to each program (<base>.nc -> <base>.json), parsed down to the fields
// the setup sheet uses. Lengths are converted to inches here, once, so
// nothing downstream has to know the library was metric.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CardTool is one tool of a program, in the program's run order.
// Number is the pocket. Lengths are inches; TaperAngle is radians.
type CardTool struct {
	Number       int     `json:"number"`
	Description  string  `json:"description"`
	Type         string  `json:"type"`
	Vendor       string  `json:"vendor"`
	ProductID    string  `json:"productId"`
	Diameter     float64 `json:"diameter"`
	CornerRadius float64 `json:"cornerRadius"`
	TaperAngle   float64 `json:"taperAngle"`
	Flutes       int     `json:"flutes"`
	FluteLength  float64 `json:"fluteLength"`
	BodyLength   float64 `json:"bodyLength"`
	Holder       string  `json:"holder"`
	IsDrill      bool    `json:"isDrill"`
}

// Cards is a parsed sidecar.
type Cards struct {
	Name    string
	Comment string
	Tools   []CardTool
}

// ParseCards parses a gmw-nc-cards/1 sidecar, converting mm tools to inches.
func ParseCards(buf []byte) (*Cards, error) {
	var raw struct {
		Schema  string `json:"schema"`
		Program struct {
			Name    string `json:"name"`
			Comment string `json:"comment"`
			Unit    string `json:"unit"`
		} `json:"program"`
		Tools []CardTool `json:"tools"`
	}
	if err := json.Unmarshal(buf, &raw); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(raw.Schema, "gmw-nc-cards/") {
		return nil, fmt.Errorf("not a gmw-nc-cards sidecar (schema %q)", raw.Schema)
	}
	scale := 1.0
	switch strings.ToLower(raw.Program.Unit) {
	case "mm", "millimeter", "millimeters":
		scale = 1 / 25.4
	}
	for i := range raw.Tools {
		t := &raw.Tools[i]
		t.Diameter *= scale
		t.CornerRadius *= scale
		t.FluteLength *= scale
		t.BodyLength *= scale
	}
	return &Cards{Name: raw.Program.Name, Comment: raw.Program.Comment, Tools: raw.Tools}, nil
}

// ProgramTools returns the tools the setup sheet lists: run order, each
// pocket once (first use wins), without T19 (chip fan) and T20 (probe).
func (c *Cards) ProgramTools() []CardTool {
	seen := map[int]bool{}
	var out []CardTool
	for _, t := range c.Tools {
		if t.Number < 1 || t.Number == 19 || t.Number == 20 || seen[t.Number] {
			continue
		}
		seen[t.Number] = true
		out = append(out, t)
	}
	return out
}
