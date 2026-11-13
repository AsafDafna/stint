package attribute

import (
	"net"
	"net/url"
	"strings"
)

// Row key prefixes.
const (
	KeyHost = "host:"
	KeyApp  = "app:"
)

// RowKey is the stretch pop-up row a sample belongs to: "host:<host>" when its URL is http(s)
// with a host (lowercased, port dropped, leading "www." stripped), else "app:<bundle id>"
// (chrome://, chrome-extension:// and file:// pages and URL-less samples included).
func RowKey(s Sample) string {
	if u, err := url.Parse(s.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		if host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."); host != "" {
			return KeyHost + host
		}
	}
	return KeyApp + s.App
}

// NoAlwaysApps never offer "always": an `app = <browser>` rule at the end of rules.toml would
// claim every unmatched site, and browsers the collector does not read never carry a URL.
var NoAlwaysApps = map[string]bool{
	"com.apple.Safari":           true,
	"company.thebrowser.Browser": true, // Arc
	"org.mozilla.firefox":        true,
	"com.brave.Browser":          true,
	"com.microsoft.edgemac":      true,
	"com.google.Chrome":          true,
	"com.bookry.wavebox":         true,
	"com.officecommun.search":    true,
}

// NoAlways reports whether a row key must not become a learned rule: a localhost or
// IP host, also with an FQDN's trailing dot (dev servers serve several projects); an app in
// NoAlwaysApps, an app in urlApps (it carried a URL or a cwd: the browsers and terminals the
// collector reads), or an empty bundle id (an `app = ""` rule would not load).
func NoAlways(key string, urlApps map[string]bool) bool {
	if host, ok := strings.CutPrefix(key, KeyHost); ok {
		host = strings.TrimSuffix(host, ".")
		return host == "localhost" || net.ParseIP(host) != nil
	}
	app := strings.TrimPrefix(key, KeyApp)
	return app == "" || NoAlwaysApps[app] || urlApps[app]
}
