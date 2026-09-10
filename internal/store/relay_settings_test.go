package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRelaySettingsDefaultDisabled(t *testing.T) {
	db := newTestStore(t)
	rs, err := db.GetRelaySettings(context.Background())
	if err != nil {
		t.Fatalf("GetRelaySettings: %v", err)
	}
	if rs.Enabled {
		t.Error("relay should default to disabled when never configured")
	}
}

func TestRelaySettingsRoundTrip(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	err := db.SetRelaySettings(ctx, RelaySettings{
		Enabled: true, Host: "smtp-relay.brevo.com", Port: 587,
		Username: "alice@example.com", Password: "secret-api-key",
	})
	if err != nil {
		t.Fatalf("SetRelaySettings: %v", err)
	}

	rs, err := db.GetRelaySettings(ctx)
	if err != nil {
		t.Fatalf("GetRelaySettings: %v", err)
	}
	if !rs.Enabled || rs.Host != "smtp-relay.brevo.com" || rs.Port != 587 ||
		rs.Username != "alice@example.com" || rs.Password != "secret-api-key" {
		t.Errorf("round-tripped settings = %+v, want the values just saved", rs)
	}
}

func TestRelaySettingsBlankPasswordPreservesExisting(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if err := db.SetRelaySettings(ctx, RelaySettings{
		Enabled: true, Host: "smtp-relay.brevo.com", Port: 587,
		Username: "alice@example.com", Password: "original-secret",
	}); err != nil {
		t.Fatalf("initial SetRelaySettings: %v", err)
	}

	// Admin edits host but leaves the password field blank ("unchanged").
	if err := db.SetRelaySettings(ctx, RelaySettings{
		Enabled: true, Host: "smtp2.example.com", Port: 2525,
		Username: "alice@example.com", Password: "",
	}); err != nil {
		t.Fatalf("second SetRelaySettings: %v", err)
	}

	rs, err := db.GetRelaySettings(ctx)
	if err != nil {
		t.Fatalf("GetRelaySettings: %v", err)
	}
	if rs.Password != "original-secret" {
		t.Errorf("Password = %q, want the original preserved when the update left it blank", rs.Password)
	}
	if rs.Host != "smtp2.example.com" || rs.Port != 2525 {
		t.Errorf("Host/Port = %q/%d, want the newly saved values", rs.Host, rs.Port)
	}
}

func TestRelaySettingsCanBeDisabledAgain(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	_ = db.SetRelaySettings(ctx, RelaySettings{Enabled: true, Host: "h", Port: 587, Username: "u", Password: "p"})
	if err := db.SetRelaySettings(ctx, RelaySettings{Enabled: false, Host: "h", Port: 587, Username: "u"}); err != nil {
		t.Fatalf("SetRelaySettings: %v", err)
	}

	rs, err := db.GetRelaySettings(ctx)
	if err != nil {
		t.Fatalf("GetRelaySettings: %v", err)
	}
	if rs.Enabled {
		t.Error("relay should be disabled after explicitly turning it off")
	}
}
