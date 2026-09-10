package webmail

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/sociolytik/mailserver/internal/address"
	"github.com/sociolytik/mailserver/internal/imapbackend"
	"github.com/sociolytik/mailserver/internal/maildir"
	"github.com/sociolytik/mailserver/internal/store"
)

// adminUserRow is one row of the mailbox directory shown on the Admin page.
type adminUserRow struct {
	ID      int64
	Email   string
	IsAdmin bool
}

// relayView is what the admin page shows/edits for the outbound smart
// host. It deliberately excludes the stored password — the form's
// password field is always blank on load (see handleAdminRelaySettings),
// so the secret is never echoed back into rendered HTML.
type relayView struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
}

type adminData struct {
	Email   string // the signed-in account's own email, for the sidebar footer
	IsAdmin bool   // always true to reach this page, but templates share the sidebar
	Users   []adminUserRow
	Relay   relayView
	Error   string
	Success string
}

// requireAdmin wraps an accountHandler so it 403s for any signed-in account
// that isn't an admin, instead of just requiring any authenticated session.
func (s *Server) requireAdmin(h accountHandler) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
		if !account.IsAdmin() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r, account)
	})
}

func (s *Server) buildAdminData(r *http.Request, account *imapbackend.Account) (*adminData, error) {
	users, err := s.Backend.Store.ListUsers(r.Context())
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	rows := make([]adminUserRow, len(users))
	for i, u := range users {
		rows[i] = adminUserRow{ID: u.ID, Email: u.Email, IsAdmin: u.IsAdmin}
	}

	rs, err := s.Backend.Store.GetRelaySettings(r.Context())
	if err != nil {
		return nil, fmt.Errorf("loading relay settings: %w", err)
	}

	return &adminData{
		Email:   account.Email(),
		IsAdmin: true,
		Users:   rows,
		Relay:   relayView{Enabled: rs.Enabled, Host: rs.Host, Port: rs.Port, Username: rs.Username},
	}, nil
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	data, err := s.buildAdminData(r, account)
	if err != nil {
		s.Logger.Error("building admin page", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	render(w, "admin_page", data)
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")
	makeAdmin := r.FormValue("is_admin") == "on"

	data, err := s.buildAdminData(r, account)
	if err != nil {
		s.Logger.Error("building admin page", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	local, domain, ok := address.Split(email)
	switch {
	case email == "" || password == "":
		data.Error = "Email and password are required"
	case !ok:
		data.Error = fmt.Sprintf("Invalid email address %q", email)
	case !s.Backend.Config.IsLocalDomain(domain):
		data.Error = fmt.Sprintf("Domain %q isn't configured on this server", domain)
	default:
		if _, err := s.Backend.Store.GetUserByEmail(r.Context(), email); err == nil {
			data.Error = fmt.Sprintf("%s already exists", email)
			break
		}

		u, err := s.Backend.Store.CreateUser(r.Context(), domain, local, password, makeAdmin)
		if err != nil {
			s.Logger.Error("creating mailbox", "email", email, "error", err)
			data.Error = "Failed to create mailbox: " + err.Error()
			break
		}
		if err := imapbackend.ProvisionMailboxes(r.Context(), s.Backend.Config, s.Backend.Store, u); err != nil {
			s.Logger.Error("provisioning mailboxes", "email", email, "error", err)
			data.Error = "Mailbox created, but folder setup failed: " + err.Error()
			break
		}

		refreshed, err := s.buildAdminData(r, account)
		if err == nil {
			data = refreshed
		}
		data.Success = fmt.Sprintf("Created mailbox %s", email)
	}

	render(w, "admin_page", data)
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	data, err := s.buildAdminData(r, account)
	if err != nil {
		s.Logger.Error("building admin page", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	target, err := s.Backend.Store.GetUserByID(r.Context(), id)
	if err != nil {
		data.Error = "Mailbox not found"
		render(w, "admin_page", data)
		return
	}

	// Blocking self-delete unconditionally (rather than only when you're the
	// last admin) is what actually prevents locking every admin out: since
	// only an admin can reach this handler, the one scenario that would zero
	// out the admin count is always a self-delete — deleting a *different*
	// admin can only ever reduce the count to 1 (yourself), never to 0. If
	// you need to remove your own mailbox, have another admin do it.
	if strings.EqualFold(target.Email, account.Email()) {
		data.Error = "You can't delete the mailbox you're currently signed in as"
		render(w, "admin_page", data)
		return
	}

	if err := s.Backend.Store.DeleteUser(r.Context(), id); err != nil {
		s.Logger.Error("deleting mailbox", "email", target.Email, "error", err)
		data.Error = "Failed to delete mailbox: " + err.Error()
		render(w, "admin_page", data)
		return
	}

	local, domain, _ := address.Split(target.Email)
	root := maildir.UserRoot(s.Backend.Config.Storage.MaildirPath, domain, local)
	if err := os.RemoveAll(root); err != nil {
		// The directory entry is already gone; losing the on-disk cleanup
		// isn't worth failing the request over, but it is worth logging so
		// an operator can clean it up by hand.
		s.Logger.Error("removing maildir after user delete", "email", target.Email, "path", root, "error", err)
	}

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminRelaySettings saves the outbound smart-host configuration
// (Brevo, or any other authenticated SMTP relay — the form works
// identically regardless of provider). Leaving the password field blank
// keeps whatever's already stored, since the form never echoes it back.
func (s *Server) handleAdminRelaySettings(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	data, err := s.buildAdminData(r, account)
	if err != nil {
		s.Logger.Error("building admin page", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	enabled := r.FormValue("relay_enabled") == "on"
	host := strings.TrimSpace(r.FormValue("relay_host"))
	username := strings.TrimSpace(r.FormValue("relay_username"))
	password := r.FormValue("relay_password")
	port := 587
	if p := strings.TrimSpace(r.FormValue("relay_port")); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			port = n
		} else {
			data.Error = "Port must be a number between 1 and 65535"
			data.Relay = relayView{Enabled: enabled, Host: host, Port: port, Username: username}
			render(w, "admin_page", data)
			return
		}
	}

	if enabled && (host == "" || username == "") {
		data.Error = "Host and username are required to enable the relay"
		data.Relay = relayView{Enabled: enabled, Host: host, Port: port, Username: username}
		render(w, "admin_page", data)
		return
	}

	err = s.Backend.Store.SetRelaySettings(r.Context(), store.RelaySettings{
		Enabled: enabled, Host: host, Port: port, Username: username, Password: password,
	})
	if err != nil {
		s.Logger.Error("saving relay settings", "error", err)
		data.Error = "Failed to save relay settings: " + err.Error()
		render(w, "admin_page", data)
		return
	}

	refreshed, err := s.buildAdminData(r, account)
	if err == nil {
		data = refreshed
	}
	if enabled {
		data.Success = "Outbound relay saved and enabled. New mail will route through it starting with the next delivery attempt."
	} else {
		data.Success = "Outbound relay saved and disabled. Mail will deliver directly to each recipient's mail server again."
	}
	render(w, "admin_page", data)
}
