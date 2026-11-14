package webmail

import "testing"

func TestDisplayName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"NRS REV360 <rev360donotreply@nrs.example>", "NRS REV360"},
		{`"Doe, Jo" <jo@x.com>`, "Doe, Jo"},
		{"bare@x.com", "bare@x.com"},
		{"<bare@x.com>", "bare@x.com"},
		{"A <a@x.com>, b@y.com", "A, b@y.com"},
		{"", ""},
		{"not an address at all", "not an address at all"},
	}
	for _, tc := range tests {
		if got := displayName(tc.in); got != tc.want {
			t.Errorf("displayName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDisplayAddr(t *testing.T) {
	if got := displayAddr("A <a@x.com>, B <b@y.com>"); got != "a@x.com, b@y.com" {
		t.Errorf("got %q", got)
	}
	if got := displayAddr("junk"); got != "junk" {
		t.Errorf("got %q", got)
	}
}

func TestAvatarInitial(t *testing.T) {
	tests := []struct{ in, want string }{
		{"NRS REV360 <r@x.com>", "N"},
		{"segun orimakinde <s@x.com>", "S"},
		{"<42@x.com>", "4"},
		{`"  ..." <z@x.com>`, "Z"},
		{"", "?"},
	}
	for _, tc := range tests {
		if got := avatarInitial(tc.in); got != tc.want {
			t.Errorf("avatarInitial(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAvatarClassStable(t *testing.T) {
	a := avatarClass("Same Person <same@x.com>")
	if a != avatarClass("different name <SAME@x.com>") {
		t.Error("same address should give the same class regardless of name/case")
	}
	if len(a) != 3 || a[:2] != "av" {
		t.Errorf("unexpected class %q", a)
	}
}
