// Package report renders engine reports for humans (text) and machines
// (JSON).
package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/emre-tiryaki/gostest/internal/engine"
)

// TextOptions tunes the text report.
type TextOptions struct {
	Color   bool
	Verbose bool
	Context int
}

// Text writes a human readable report.
func Text(w io.Writer, rep engine.Report, opt TextOptions) error {
	for _, o := range rep.Outcomes {
		if _, err := fmt.Fprintf(w, "%s %s\n", o.Status, o.Distro.Name); err != nil {
			return err
		}
	}
	return nil
}

// JSON writes the report as JSON.
func JSON(w io.Writer, rep engine.Report) error {
	return json.NewEncoder(w).Encode(rep)
}
