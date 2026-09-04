package httpapi

import (
	"embed"
	"io/fs"
	"net/http"
)

// The demo page is embedded rather than served from disk: one binary, no asset path to
// get wrong in a container.
//
//go:embed web
var webFS embed.FS

// DemoUI serves the single-page demo.
//
// It is deliberately not a chat widget. A widget's job is to make the model feel
// seamless and invisible; the substance here *is* the invisible part -- which passages
// retrieval found and how they scored, which tools ran and what they decided, and how
// many model calls the turn actually billed for. One file, no build step.
func DemoUI() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // The embed directive above guarantees this directory exists.
	}
	return http.FileServer(http.FS(sub))
}
