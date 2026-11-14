package webmail

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// The browser-side code (CSS, htmx, the compose island, keyboard shortcuts)
// is built by Vite from /web into static/dist. This file turns Vite's
// manifest.json into the <link>/<script> tags the page templates need, so
// pages reference content-hashed filenames without hard-coding them.

const (
	distURLPrefix = "/static/dist/"
	devEnvVar     = "MAILSERVER_WEBMAIL_DEV"
)

type manifestEntry struct {
	File    string   `json:"file"`
	CSS     []string `json:"css"`
	Imports []string `json:"imports"` // static imports only; dynamic ones load on demand
}

type assetSet struct {
	manifest map[string]manifestEntry
	dev      atomic.Bool // emit Vite dev-server tags instead of built files
	loadErr  error
}

// loadAssets reads dist/manifest.json from fsys (the embedded static dir). A
// missing manifest isn't fatal — `go build` and the tests work without
// running Node — it just means pages render without styles or scripts, which
// tags() reports in an HTML comment.
func loadAssets(fsys fs.FS) *assetSet {
	a := &assetSet{}
	data, err := fs.ReadFile(fsys, "dist/manifest.json")
	if err != nil {
		a.loadErr = fmt.Errorf("reading dist/manifest.json: %w", err)
		return a
	}
	if err := json.Unmarshal(data, &a.manifest); err != nil {
		a.loadErr = fmt.Errorf("parsing dist/manifest.json: %w", err)
	}
	return a
}

// tags returns the HTML that loads the named Vite entry (e.g. "src/main.ts").
func (a *assetSet) tags(entry string) template.HTML {
	if a.dev.Load() {
		// Dev: Vite serves modules and HMR; it injects CSS through JS.
		return template.HTML(`<script type="module" src="/@vite/client"></script>` +
			`<script type="module" src="/` + html.EscapeString(entry) + `"></script>`)
	}

	e, ok := a.manifest[entry]
	if !ok {
		reason := "entry " + entry + " not in manifest"
		if a.loadErr != nil {
			reason = a.loadErr.Error()
		}
		return template.HTML("<!-- webmail assets not built (" + html.EscapeString(reason) + "): run `npm run build` in web/ -->")
	}

	var b strings.Builder
	seen := map[string]bool{}
	var css, preload []string
	var walk func(name string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		ent, ok := a.manifest[name]
		if !ok {
			return
		}
		css = append(css, ent.CSS...)
		for _, imp := range ent.Imports {
			if ie, ok := a.manifest[imp]; ok {
				preload = append(preload, ie.File)
			}
			walk(imp)
		}
	}
	walk(entry)

	for _, c := range css {
		fmt.Fprintf(&b, `<link rel="stylesheet" href="%s%s">`, distURLPrefix, html.EscapeString(c))
	}
	fmt.Fprintf(&b, `<script type="module" src="%s%s"></script>`, distURLPrefix, html.EscapeString(e.File))
	for _, p := range preload {
		fmt.Fprintf(&b, `<link rel="modulepreload" href="%s%s">`, distURLPrefix, html.EscapeString(p))
	}
	return template.HTML(b.String())
}

// assets is the process-wide asset set the "assets" template func reads.
var assets = loadAssets(mustSubStatic())

func mustSubStatic() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// configureAssets applies runtime settings and logs asset problems once at
// startup instead of on every page view.
func configureAssets(logger *slog.Logger) {
	if os.Getenv(devEnvVar) != "" {
		assets.dev.Store(true)
		logger.Info("webmail serving Vite dev-server assets; browse the Vite port (default http://localhost:5173)")
		return
	}
	if assets.loadErr != nil {
		logger.Error("webmail frontend assets are missing; pages will render unstyled. Build them with `npm run build` in web/", "error", assets.loadErr)
	}
}
