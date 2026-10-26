package util

import "testing"

func TestPlainSnippet(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"<b>Bold</b> tale<br>of &amp; things", 100, "Bold tale of & things"},
		{"one  two\n\nthree", 100, "one two three"},
		{"alpha beta gamma", 10, "alpha..."},
		{"", 10, ""},
	} {
		if got := PlainSnippet(tc.in, tc.max); got != tc.want {
			t.Fatalf("PlainSnippet(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestPlainDescription(t *testing.T) {
	in := "Mika is a draker.<br><br> (Source: Kodansha USA)"
	want := "Mika is a draker.\n\n(Source: Kodansha USA)"
	if got := PlainDescription(in); got != want {
		t.Fatalf("PlainDescription = %q, want %q", got, want)
	}
	if got := PlainDescription("a<br/>b<BR >c &amp; <i>d</i>"); got != "a\nb\nc & d" {
		t.Fatalf("variants = %q", got)
	}
}
