package mcp

import "testing"

// The CLI prints the URL as an OSC-8 hyperlink: an invisible copy in the escape's
// payload, then the coloured visible copy. Stripping only the CSI colours glued
// the two copies together with ]8;; litter in between — which is what the auth
// card then displayed, and what the sweep would have opened.
func TestFindAuthURLThroughTerminalNoise(t *testing.T) {
	url := "https://mcp.grafana.com/mcp/oauth/authorize?response_type=code&state=abc"
	osc8 := "Visit this URL to authorize:\n  \x1b]8;;" + url + "\x07\x1b[94m" + url + "\x1b[39m\x1b]8;;\x07\n\nWaiting…"
	if got := FindAuthURL(osc8); got != url {
		t.Errorf("osc8: got %q", got)
	}
	// The ST (ESC \) terminator form, and a plain untangled URL, both still work.
	st := "\x1b]8;;" + url + "\x1b\\" + url + "\x1b]8;;\x1b\\"
	if got := FindAuthURL(st); got != url {
		t.Errorf("st: got %q", got)
	}
	if got := FindAuthURL("go to " + url + ", then wait"); got != url {
		t.Errorf("plain: got %q", got)
	}
	if got := FindAuthURL("no url here"); got != "" {
		t.Errorf("none: got %q", got)
	}
}
