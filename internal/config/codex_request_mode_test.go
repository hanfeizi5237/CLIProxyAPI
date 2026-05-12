package config

import "testing"

func TestNormalizeCodexRequestMode(t *testing.T) {
	cases := map[string]string{
		"":            "responses",
		"responses":   "responses",
		"RESPONSES":   "responses",
		"chat":        "chat",
		" CHAT ":      "chat",
		"unsupported": "responses",
	}

	for input, want := range cases {
		if got := NormalizeCodexRequestMode(input); got != want {
			t.Fatalf("NormalizeCodexRequestMode(%q) = %q, want %q", input, got, want)
		}
	}
}
