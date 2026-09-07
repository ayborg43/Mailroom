package ratelimit

import (
	"testing"
	"time"
)

func TestAllowedByDefault(t *testing.T) {
	l := New(3, time.Minute, time.Minute)
	if !l.Allowed("1.2.3.4") {
		t.Fatal("a never-seen key should be allowed")
	}
}

func TestBansAfterMaxAttempts(t *testing.T) {
	l := New(3, time.Minute, time.Hour)
	key := "1.2.3.4"

	l.RecordFailure(key)
	l.RecordFailure(key)
	if !l.Allowed(key) {
		t.Fatal("should still be allowed below MaxAttempts")
	}

	l.RecordFailure(key)
	if l.Allowed(key) {
		t.Fatal("should be banned after reaching MaxAttempts")
	}
}

func TestSuccessResetsCounter(t *testing.T) {
	l := New(3, time.Minute, time.Hour)
	key := "1.2.3.4"

	l.RecordFailure(key)
	l.RecordFailure(key)
	l.RecordSuccess(key)
	l.RecordFailure(key)
	if !l.Allowed(key) {
		t.Fatal("a success should reset the failure count, not carry it toward a ban")
	}
}

func TestBanExpires(t *testing.T) {
	l := New(1, time.Minute, 10*time.Millisecond)
	key := "1.2.3.4"

	l.RecordFailure(key)
	if l.Allowed(key) {
		t.Fatal("should be banned immediately after one failure with MaxAttempts=1")
	}

	time.Sleep(30 * time.Millisecond)
	if !l.Allowed(key) {
		t.Fatal("ban should have expired")
	}
}

func TestFailuresOutsideWindowDontAccumulate(t *testing.T) {
	l := New(2, 20*time.Millisecond, time.Hour)
	key := "1.2.3.4"

	l.RecordFailure(key)
	time.Sleep(30 * time.Millisecond) // first failure ages out of the window
	l.RecordFailure(key)
	if !l.Allowed(key) {
		t.Fatal("only one failure should still be inside the window, below MaxAttempts")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(1, time.Minute, time.Hour)
	l.RecordFailure("1.2.3.4")
	if !l.Allowed("5.6.7.8") {
		t.Fatal("banning one key must not affect a different key")
	}
}

func TestCleanupRemovesStaleEntries(t *testing.T) {
	l := New(5, 10*time.Millisecond, 10*time.Millisecond)
	l.RecordFailure("1.2.3.4")

	time.Sleep(30 * time.Millisecond)
	l.cleanup()

	l.mu.Lock()
	_, exists := l.entries["1.2.3.4"]
	l.mu.Unlock()
	if exists {
		t.Fatal("expired, unbanned entry should have been cleaned up")
	}
}
