package attribute

import "testing"

func TestRowKey(t *testing.T) {
	cases := []struct {
		name string
		in   Sample
		want string
	}{
		{"https host lowercased, www. and port dropped", Sample{App: "com.google.Chrome", URL: "https://WWW.Grok.com:8443/chat?q=1"}, "host:grok.com"},
		{"http sub-domain kept", Sample{App: "com.bookry.wavebox", URL: "http://app.acme.example/x"}, "host:app.acme.example"},
		{"upper-case scheme", Sample{App: "com.google.Chrome", URL: "HTTPS://grok.com/"}, "host:grok.com"},
		{"chrome:// page is the app", Sample{App: "com.google.Chrome", URL: "chrome://settings"}, "app:com.google.Chrome"},
		{"chrome-extension:// page is the app", Sample{App: "com.google.Chrome", URL: "chrome-extension://abc/popup.html"}, "app:com.google.Chrome"},
		{"file:// page is the app", Sample{App: "com.google.Chrome", URL: "file:///Users/me/x.pdf"}, "app:com.google.Chrome"},
		{"URL-less browser sample is the app", Sample{App: "com.apple.Safari"}, "app:com.apple.Safari"},
		{"plain app", Sample{App: "net.whatsapp.WhatsApp", Title: "WhatsApp"}, "app:net.whatsapp.WhatsApp"},
		{"IPv6 loopback keeps its host", Sample{App: "com.google.Chrome", URL: "http://[::1]:3000/"}, "host:::1"},
		{"FQDN keeps its trailing dot", Sample{App: "com.google.Chrome", URL: "https://grok.com./chat"}, "host:grok.com."},
		{"bare www. host is the app", Sample{App: "com.google.Chrome", URL: "https://www./"}, "app:com.google.Chrome"},
		{"unparseable URL falls back to the app", Sample{App: "X", URL: "%zz"}, "app:X"},
		{"no app at all", Sample{}, "app:"},
	}
	for _, c := range cases {
		if got := RowKey(c.in); got != c.want {
			t.Errorf("%s: RowKey = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNoAlways(t *testing.T) {
	urlApps := map[string]bool{"com.mitchellh.ghostty": true}
	cases := []struct {
		key  string
		want bool
	}{
		{"host:grok.com", false},
		{"host:localhost", true},
		{"host:localhost.", true},
		{"host:127.0.0.1", true},
		{"host:127.0.0.1.", true},
		{"host:grok.com.", false},
		{"host:192.168.1.20", true},
		{"host:::1", true},
		{"app:com.officecommun.search", true},
		{"app:com.apple.Safari", true},      // static browser list, no URL samples needed
		{"app:com.mitchellh.ghostty", true}, // URL or cwd history
		{"app:net.whatsapp.WhatsApp", false},
		{"app:", true},
	}
	for _, c := range cases {
		if got := NoAlways(c.key, urlApps); got != c.want {
			t.Errorf("NoAlways(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}
