package output

import (
	"encoding/json"
	"fmt"
	"io"

	"schoolwork-check/internal/model"
)

// WriteJSON writes the tasks as a pretty-printed JSON array, sorted by Sort.
// Field order follows the model.Task struct definition.
func WriteJSON(w io.Writer, tasks []model.Task) error {
	out := sorted(tasks)
	if out == nil {
		out = []model.Task{}
	}
	// Downstream consumers expect arrays, never null.
	for i := range out {
		if out[i].Attachments == nil {
			out[i].Attachments = []model.Attachment{}
		}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("output: write json: %w", err)
	}
	return nil
}
