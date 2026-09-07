package webmail

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sociolytik/mailserver/internal/imapbackend"
)

type pageData struct {
	Email         string
	IsAdmin       bool
	Folders       []imapbackend.FolderInfo
	CurrentFolder string
	Messages      []imapbackend.MessageSummary
	Page          int
	TotalPages    int
	Total         int
	Query         string
	Selected      *imapbackend.FullMessage
	SelectedUID   uint32
}

func isHTMXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

func (s *Server) buildPageData(ctx context.Context, account *imapbackend.Account, folder string, selectedUID uint32, page int, query string) (*pageData, error) {
	folders, err := account.Folders(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing folders: %w", err)
	}

	var messages []imapbackend.MessageSummary
	total := 0
	if query != "" {
		messages, err = account.Search(folder, query)
		if err != nil {
			return nil, fmt.Errorf("searching: %w", err)
		}
		total = len(messages)
	} else {
		messages, total, err = account.ListMessages(folder, page, defaultPageSize)
		if err != nil {
			return nil, fmt.Errorf("listing messages: %w", err)
		}
	}

	var selected *imapbackend.FullMessage
	if selectedUID != 0 {
		// Tolerate a stale/invalid UID (e.g. the message was deleted from
		// another session) rather than failing the whole page load.
		selected, _ = account.GetMessage(folder, selectedUID)
	}

	totalPages := (total + defaultPageSize - 1) / defaultPageSize
	if totalPages < 1 {
		totalPages = 1
	}

	return &pageData{
		Email:         account.Email(),
		IsAdmin:       account.IsAdmin(),
		Folders:       folders,
		CurrentFolder: folder,
		Messages:      messages,
		Page:          page,
		TotalPages:    totalPages,
		Total:         total,
		Query:         query,
		Selected:      selected,
		SelectedUID:   selectedUID,
	}, nil
}

func parsePage(r *http.Request) int {
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	return page
}

func parseUID(r *http.Request) (uint32, bool) {
	n, err := strconv.ParseUint(r.PathValue("uid"), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

func (s *Server) handleFolder(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	folder := r.PathValue("folder")
	page := parsePage(r)

	data, err := s.buildPageData(r.Context(), account, folder, 0, page, "")
	if err != nil {
		s.Logger.Error("building folder page", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if isHTMXRequest(r) {
		render(w, "message_list", data)
		return
	}
	render(w, "mail_page", data)
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	folder := r.PathValue("folder")
	uid, ok := parseUID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	data, err := s.buildPageData(r.Context(), account, folder, uid, parsePage(r), "")
	if err != nil {
		s.Logger.Error("building message page", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if isHTMXRequest(r) {
		render(w, "message_view", data)
		return
	}
	render(w, "mail_page", data)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	folder := r.PathValue("folder")
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	data, err := s.buildPageData(r.Context(), account, folder, 0, 1, query)
	if err != nil {
		s.Logger.Error("searching", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	render(w, "message_list", data)
}

func (s *Server) handleFlag(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	folder := r.PathValue("folder")
	uid, ok := parseUID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	flag := r.FormValue("flag")

	sum, err := account.GetSummary(folder, uid)
	if err != nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if err := account.SetFlags(folder, uid, toggleFlag(sum.Flags, flag)); err != nil {
		s.Logger.Error("setting flags", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/mail/%s/%d", folder, uid), http.StatusSeeOther)
}

func toggleFlag(flags []string, flag string) []string {
	out := make([]string, 0, len(flags)+1)
	found := false
	for _, f := range flags {
		if strings.EqualFold(f, flag) {
			found = true
			continue
		}
		out = append(out, f)
	}
	if !found {
		out = append(out, flag)
	}
	return out
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	folder := r.PathValue("folder")
	uid, ok := parseUID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if err := account.Delete(folder, uid); err != nil {
		s.Logger.Error("deleting message", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/mail/"+folder, http.StatusSeeOther)
}
