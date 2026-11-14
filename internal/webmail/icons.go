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

	"reply":     `<path d="M9 17 4 12l5-5"/><path d="M20 18v-2a4 4 0 0 0-4-4H4"/>`,
	"reply-all": `<path d="m7 17-5-5 5-5"/><path d="m12 17-5-5 5-5"/><path d="M22 18v-2a4 4 0 0 0-4-4H7"/>`,
	"forward":   `<path d="m15 17 5-5-5-5"/><path d="M4 18v-2a4 4 0 0 1 4-4h12"/>`,
	"flag":      `<path d="M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z"/><path d="M4 22v-7"/>`,
	"paperclip": `<path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48"/>`,
	"keyboard":  `<rect width="20" height="14" x="2" y="5" rx="2"/><path d="M6 9h.01M10 9h.01M14 9h.01M18 9h.01M8 13h8"/>`,
	"user":      `<path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/>`,
	"shield":    `<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/>`,
	"sun":       `<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41"/>`,
	"moon":      `<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>`,
	"monitor":   `<rect width="20" height="14" x="2" y="3" rx="2"/><path d="M8 21h8M12 17v4"/>`,
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
