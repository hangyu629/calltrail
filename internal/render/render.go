// Package render is the view layer for the self-built viewer: it inlines a model.json into one
// self-contained HTML file. (Other views, e.g. an Archify export, would be siblings of this.)
package render

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/hangyu629/calltrail/internal/model"
)

//go:embed viewer.html
var viewer []byte

// elkjs 0.12.1 (EPL-2.0, see ELK-LICENSE.md): layered layout with nested groups and edge routing.
//
//go:embed elk.bundled.js
var elkJS []byte

const placeholder = "/*__GRAPH_JSON__*/null"
const elkPlaceholder = "/*__ELK_JS__*/"

func HTML(m *model.Model) ([]byte, error) {
	if !bytes.Contains(viewer, []byte(placeholder)) {
		return nil, fmt.Errorf("viewer template lost its placeholder")
	}
	// json.Marshal escapes <, > and &, so the payload cannot close the <script> tag.
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(elkJS, []byte("</script")) {
		return nil, fmt.Errorf("elk bundle contains </script, cannot inline")
	}
	out := bytes.Replace(viewer, []byte(elkPlaceholder), elkJS, 1)
	return bytes.Replace(out, []byte(placeholder), data, 1), nil
}
