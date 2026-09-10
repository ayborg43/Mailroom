package webmail

import (
	"net"
	"net/http"

	"github.com/sociolytik/mailserver/internal/imapbackend"
)

type accountData struct {
	Email   string
	IsAdmin bool
	Error   string

	// Connection settings for configuring a mail client (Thunderbird,
	// Outlook, Apple Mail) or any app that sends mail via authenticated
	// SMTP submission — same host/ports for every account, just shown
	// here so a user doesn't need to go ask an admin for them.
	Hostname string
	IMAPPort string
	SMTPPort string
}

func (s *Server) newAccountData(account *imapbackend.Account) accountData {
	return accountData{
		Email:    account.Email(),
		IsAdmin:  account.IsAdmin(),
		Hostname: s.Hostname,
		IMAPPort: portFromAddr(s.Backend.Config.Listen.IMAP),
		SMTPPort: portFromAddr(s.Backend.Config.Listen.Submission),
	}
}

// portFromAddr extracts the port from a listen address like ":993" or
// "0.0.0.0:587" — what a mail client needs is the bare port number to pair
// with the server's public hostname, not whatever local bind address the
// server happens to listen on.
func portFromAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return port
}

func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	render(w, "account_page", s.newAccountData(account))
}

func (s *Server) handleAccountPasswordChange(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	current := r.FormValue("current_password")
	newPassword := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	data := s.newAccountData(account)

	switch {
	case current == "" || newPassword == "":
		data.Error = "Current and new password are required"
	case len(newPassword) < 8:
		data.Error = "New password must be at least 8 characters"
	case newPassword != confirm:
		data.Error = "New password and confirmation don't match"
	default:
		if err := account.ChangePassword(r.Context(), current, newPassword); err != nil {
			data.Error = "Failed to change password: " + err.Error()
			break
		}

		// Force re-authentication: this session (and any other active one
		// for this account) still holds the account object from before the
		// change, and sessions aren't otherwise invalidated on password
		// change — signing out here is the simplest way to make "changed
		// the password" actually mean something for this browser.
		if cookie, err := r.Cookie(sessionCookieName); err == nil {
			s.sessions.delete(cookie.Value)
		}
		clearSessionCookie(w)
		http.Redirect(w, r, "/login?password_changed=1", http.StatusSeeOther)
		return
	}

	render(w, "account_page", data)
}
