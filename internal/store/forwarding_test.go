package store

import (
	"context"
	"testing"
)

func TestForwardingDefaultDisabled(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "example.com", "alice", "password123", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	f, err := db.GetForwarding(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetForwarding: %v", err)
	}
	if f.Enabled || f.ForwardTo != "" {
		t.Errorf("forwarding = %+v, want disabled with no address when never configured", f)
	}
}

func TestForwardingRoundTrip(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "example.com", "alice", "password123", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := db.SetForwarding(ctx, u.ID, true, "alice@gmail.com"); err != nil {
		t.Fatalf("SetForwarding: %v", err)
	}

	f, err := db.GetForwarding(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetForwarding: %v", err)
	}
	if !f.Enabled || f.ForwardTo != "alice@gmail.com" {
		t.Errorf("forwarding = %+v, want enabled forwarding to alice@gmail.com", f)
	}
}

func TestForwardingCanBeDisabledAgain(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "example.com", "alice", "password123", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := db.SetForwarding(ctx, u.ID, true, "alice@gmail.com"); err != nil {
		t.Fatalf("initial SetForwarding: %v", err)
	}
	if err := db.SetForwarding(ctx, u.ID, false, "alice@gmail.com"); err != nil {
		t.Fatalf("SetForwarding: %v", err)
	}

	f, err := db.GetForwarding(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetForwarding: %v", err)
	}
	if f.Enabled {
		t.Error("forwarding should be disabled after explicitly turning it off")
	}
}

func TestForwardingIsPerUser(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	alice, err := db.CreateUser(ctx, "example.com", "alice", "password123", false)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, err := db.CreateUser(ctx, "example.com", "bob", "password123", false)
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	if err := db.SetForwarding(ctx, alice.ID, true, "alice@gmail.com"); err != nil {
		t.Fatalf("SetForwarding: %v", err)
	}

	bobForwarding, err := db.GetForwarding(ctx, bob.ID)
	if err != nil {
		t.Fatalf("GetForwarding bob: %v", err)
	}
	if bobForwarding.Enabled {
		t.Error("bob's forwarding rule must be unaffected by alice's")
	}
}
