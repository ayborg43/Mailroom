package webmail

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/sociolytik/mailserver/internal/imapbackend"
)

//go:embed templates/*.html
var templatesFS embed.FS

// "all:" so the static/.gitkeep placeholder counts as an embeddable file —
// that keeps the package compiling on a checkout where the frontend hasn't
// been built yet (see assets.go).
//
//go:embed all:static
var staticFS embed.FS

// rowCtx is what the "message_row" template renders from: one message plus the
// page-level context its link, selection state and OOB behaviour depend on.
type rowCtx struct {
	M           imapbackend.MessageSummary
	Folder      string
	Page        int
	SelectedUID uint32
	OOB         bool // swap this row into the existing list out of band
}

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"fmtDate": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		if time.Since(t) < 20*time.Hour && time.Now().YearDay() == t.YearDay() && time.Now().Year() == t.Year() {
			return t.Local().Format("3:04 PM")
		}
		return t.Local().Format("Jan 2")
	},
	"fmtDateLong": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("Mon, Jan 2, 2006 at 3:04 PM")
	},
	"rowCtx": func(p *pageData, m imapbackend.MessageSummary, oob bool) rowCtx {
		return rowCtx{M: m, Folder: p.CurrentFolder, Page: p.Page, SelectedUID: p.SelectedUID, OOB: oob}
	},
	"assets":     func(entry string) template.HTML { return assets.tags(entry) },
	"icon":       icon,
	"folderIcon": folderIcon,
	// json marshals a value for a data-* attribute (html/template escapes it
	// for the attribute context); islands read it as their props.
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	"add": func(a, b int) int { return a + b },
	"sub": func(a, b int) int { return a - b },
	"fmtSize": func(n int64) string {
		const unit = 1024
		if n < unit {
			return fmt.Sprintf("%d B", n)
		}
		div, exp := int64(unit), 0
		for m := n / unit; m >= unit; m /= unit {
			div *= unit
			exp++
		}
		return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
	},
}).ParseFS(templatesFS, "templates/*.html"))

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Render to nothing first isn't necessary: html/template only writes
	// once execution succeeds up to the point of an error, but partial
	// writes on error are acceptable here (a broken response beats none,
	// and errors indicate a template bug, not user-facing bad input).
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// staticHandler serves the embedded CSS/JS assets under /static/.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vite content-hashes everything under dist/assets/, so those files
		// never change at a given URL and can be cached forever. manifest.json
		// and anything else stay on default (revalidating) caching.
		if strings.HasPrefix(r.URL.Path, distURLPrefix+"assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}
