package webmail

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

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
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}
