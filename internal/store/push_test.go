package store

import (
	"context"
	"testing"
)

func TestVAPIDKeysFirstWriterWins(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if _, err := db.GetVAPIDKeys(ctx); err != ErrNotFound {
		t.Fatalf("GetVAPIDKeys on empty db = %v, want ErrNotFound", err)
	}
	first, err := db.SaveVAPIDKeysIfAbsent(ctx, VAPIDKeys{PublicKey: "pub1", PrivateKey: "priv1"})
	if err != nil || first.PublicKey != "pub1" {
		t.Fatalf("first save = %+v, %v", first, err)
	}
	second, err := db.SaveVAPIDKeysIfAbsent(ctx, VAPIDKeys{PublicKey: "pub2", PrivateKey: "priv2"})
	if err != nil {
		t.Fatal(err)
	}
	if second.PublicKey != "pub1" || second.PrivateKey != "priv1" {
		t.Errorf("second save replaced the key: %+v", second)
	}
}

func TestPushSubscriptionLifecycle(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	alice, _ := db.CreateUser(ctx, "example.com", "alice", "password123", false)
	bob, _ := db.CreateUser(ctx, "example.com", "bob", "password123", false)

	if err := db.UpsertPushSubscription(ctx, alice.ID, "https://push.example/a", "k1", "a1", "UA"); err != nil {
		t.Fatal(err)
	}
	// Re-subscribing the same endpoint updates it rather than duplicating.
	if err := db.UpsertPushSubscription(ctx, alice.ID, "https://push.example/a", "k2", "a2", "UA2"); err != nil {
		t.Fatal(err)
	}
	subs, _ := db.ListPushSubscriptions(ctx, alice.ID)
	if len(subs) != 1 || subs[0].P256dh != "k2" || subs[0].UserAgent != "UA2" {
		t.Fatalf("subs = %+v, want one updated row", subs)
	}

	// Lookup by (case-insensitive) address.
	id, byEmail, err := db.ListPushSubscriptionsByEmail(ctx, "Alice@Example.com")
	if err != nil || id != alice.ID || len(byEmail) != 1 {
		t.Fatalf("by email = %d %+v %v", id, byEmail, err)
	}
	if _, none, _ := db.ListPushSubscriptionsByEmail(ctx, "bob@example.com"); len(none) != 0 {
		t.Errorf("bob has subscriptions %+v, want none", none)
	}

	// A device that changes hands is reassigned, not shared.
	if err := db.UpsertPushSubscription(ctx, bob.ID, "https://push.example/a", "k3", "a3", "UA3"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountPushSubscriptions(ctx, alice.ID); n != 0 {
		t.Errorf("alice still has %d subscriptions after the device moved to bob", n)
	}
	if n, _ := db.CountPushSubscriptions(ctx, bob.ID); n != 1 {
		t.Errorf("bob has %d subscriptions, want 1", n)
	}

	// Users can't delete each other's.
	if ok, _ := db.DeletePushSubscription(ctx, alice.ID, "https://push.example/a"); ok {
		t.Error("alice deleted bob's subscription")
	}
	if ok, err := db.DeletePushSubscription(ctx, bob.ID, "https://push.example/a"); err != nil || !ok {
		t.Errorf("bob delete = %v, %v", ok, err)
	}
}

func TestPushSubscriptionDeleteByEndpoint(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	u, _ := db.CreateUser(ctx, "example.com", "carol", "password123", false)
	_ = db.UpsertPushSubscription(ctx, u.ID, "https://push.example/c", "k", "a", "")
	if err := db.DeletePushSubscriptionByEndpoint(ctx, "https://push.example/c"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountPushSubscriptions(ctx, u.ID); n != 0 {
		t.Errorf("count = %d after delete by endpoint", n)
	}
}
