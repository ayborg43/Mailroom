package webmail

import (
	"html/template"
	"strings"
)

// Small inline SVG icon set (24x24 grid, stroke style). Inlining keeps them
// colourable with currentColor and avoids an extra request per icon.
var iconPaths = map[string]string{
	"inbox":  `<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>`,
	"sent":   `<path d="m22 2-7 20-4-9-9-4Z"/><path d="M22 2 11 13"/>`,
	"drafts": `<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/>`,
	"trash":  `<path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>`,
	"junk":   `<circle cx="12" cy="12" r="10"/><path d="m4.9 4.9 14.2 14.2"/>`,
	"folder": `<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>`,
	"menu":   `<path d="M4 6h16M4 12h16M4 18h16"/>`,
}

func icon(name string) template.HTML {
	paths, ok := iconPaths[name]
	if !ok {
		paths = iconPaths["folder"]
	}
	return template.HTML(`<svg class="icon" viewBox="0 0 24 24" aria-hidden="true">` + paths + `</svg>`)
}

// folderIcon picks an icon for a mailbox by its (case-insensitive) name.
func folderIcon(folder string) template.HTML {
	switch strings.ToLower(folder) {
	case "inbox":
		return icon("inbox")
	case "sent", "sent items", "sent mail":
		return icon("sent")
	case "drafts", "draft":
		return icon("drafts")
	case "trash", "deleted", "deleted items", "bin":
		return icon("trash")
	case "junk", "spam", "junk email":
		return icon("junk")
	}
	return icon("folder")
}
