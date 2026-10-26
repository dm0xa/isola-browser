package main

import (
	"sort"
	"strings"
	"sync"
)

// Request classification: first-party vs third-party, known tracker families,
// and beacon requests. Like posture, this lives in Go because the panel and
// the MCP tools must agree; the panel only renders the labels.

// publicSuffixes is a compact list of multi-label suffixes. It is not the full
// Public Suffix List — it covers the endings that actually appear in captured
// traffic, and single-label TLDs need no entry (the last two labels win).
var publicSuffixes = map[string]bool{
	"ac.uk": true, "co.uk": true, "gov.uk": true, "ltd.uk": true, "me.uk": true,
	"net.uk": true, "nhs.uk": true, "org.uk": true, "plc.uk": true, "police.uk": true,
	"com.au": true, "net.au": true, "org.au": true, "edu.au": true, "gov.au": true,
	"co.nz": true, "net.nz": true, "org.nz": true,
	"co.za": true, "org.za": true, "net.za": true,
	"co.jp": true, "ne.jp": true, "or.jp": true,
	"co.kr": true, "or.kr": true,
	"com.br": true, "net.br": true, "org.br": true,
	"com.mx": true, "com.ar": true, "com.tr": true, "com.cn": true, "com.hk": true,
	"com.sg": true, "com.my": true, "com.tw": true, "com.ua": true,
	"co.in": true, "net.in": true, "org.in": true,
	"co.il": true, "org.il": true,
	"com.se": true, "org.se": true, "net.se": true, "pp.se": true,
	"com.no": true, "org.no": true, "net.no": true,
	"com.dk": true, "org.dk": true, "net.dk": true,
	"com.fi": true, "org.fi": true, "net.fi": true,
	"com.pl": true, "org.pl": true, "net.pl": true,
	"com.es": true, "org.es": true, "net.es": true,
	"com.fr": true, "org.fr": true, "net.fr": true,
	"com.de": true, "org.de": true, "net.de": true,
	"com.nl": true, "org.nl": true, "net.nl": true,
	"co.at": true, "or.at": true, "co.ch": true, "org.ch": true,
	"com.pt": true, "com.gr": true, "com.ro": true, "com.cz": true,
	"com.ru": true, "org.ru": true, "net.ru": true,
	"co.id": true, "or.id": true, "co.th": true, "in.th": true,
}

// registrableDomain returns the eTLD+1 for a host: "a.b.example.co.uk" →
// "example.co.uk". IP literals and single-label hosts are returned unchanged.
func registrableDomain(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return ""
	}
	if strings.ContainsAny(host, ":[]") || isIPv4Literal(host) {
		return host
	}
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	lastTwo := parts[len(parts)-2] + "." + parts[len(parts)-1]
	if publicSuffixes[lastTwo] && len(parts) >= 3 {
		return parts[len(parts)-3] + "." + lastTwo
	}
	return lastTwo
}

// isIPv4Literal reports whether host is a dotted quad. It deliberately does not
// resolve anything.
func isIPv4Literal(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// trackerCatalogue maps a host suffix to a human label. Suffix matching is
// label-aware: "cdn.segment.com" matches "segment.com" but "notsegment.com"
// does not.
var trackerCatalogue = []struct {
	suffix string
	label  string
}{
	{"google-analytics.com", "Google Analytics"},
	{"analytics.google.com", "Google Analytics"},
	{"googletagmanager.com", "Google Tag Manager"},
	{"doubleclick.net", "Google Ads"},
	{"googlesyndication.com", "Google Ads"},
	{"googleadservices.com", "Google Ads"},
	{"google.com/pagead", "Google Ads"},
	{"facebook.net", "Meta Pixel"},
	{"facebook.com/tr", "Meta Pixel"},
	{"connect.facebook.net", "Meta Pixel"},
	{"hotjar.com", "Hotjar"},
	{"hotjar.io", "Hotjar"},
	{"clarity.ms", "Microsoft Clarity"},
	{"segment.com", "Segment"},
	{"segment.io", "Segment"},
	{"mixpanel.com", "Mixpanel"},
	{"amplitude.com", "Amplitude"},
	{"heapanalytics.com", "Heap"},
	{"plausible.io", "Plausible"},
	{"matomo.cloud", "Matomo"},
	{"statcounter.com", "StatCounter"},
	{"intercom.io", "Intercom"},
	{"intercomcdn.com", "Intercom"},
	{"hubspot.com", "HubSpot"},
	{"hs-scripts.com", "HubSpot"},
	{"hsforms.net", "HubSpot"},
	{"drift.com", "Drift"},
	{"crisp.chat", "Crisp"},
	{"tawk.to", "Tawk.to"},
	{"zendesk.com", "Zendesk"},
	{"sentry.io", "Sentry"},
	{"bugsnag.com", "Bugsnag"},
	{"rollbar.com", "Rollbar"},
	{"newrelic.com", "New Relic"},
	{"nr-data.net", "New Relic"},
	{"datadoghq.com", "Datadog"},
	{"stripe.com", "Stripe"},
	{"js.stripe.com", "Stripe"},
	{"cloudflareinsights.com", "Cloudflare Analytics"},
	{"cdnjs.cloudflare.com", "Cloudflare CDN"},
	{"cloudflare.com", "Cloudflare"},
	{"stripe.network", "Stripe"},
	{"paypal.com", "PayPal"},
	{"klarna.com", "Klarna"},
	{"youtube.com", "YouTube"},
	{"ytimg.com", "YouTube"},
	{"vimeo.com", "Vimeo"},
	{"twitter.com", "Twitter/X"},
	{"twimg.com", "Twitter/X"},
	{"linkedin.com", "LinkedIn"},
	{"licdn.com", "LinkedIn"},
	{"tiktok.com", "TikTok"},
	{"pinterest.com", "Pinterest"},
	{"snapchat.com", "Snapchat"},
	{"recaptcha.net", "Google reCAPTCHA"},
	{"gstatic.com", "Google static"},
	{"fonts.googleapis.com", "Google Fonts"},
	{"gravatar.com", "Gravatar"},
	{"wordpress.com", "WordPress.com"},
	{"wp.com", "WordPress.com"},
	{"jsdelivr.net", "jsDelivr CDN"},
	{"unpkg.com", "unpkg CDN"},
	{"bootstrapcdn.com", "Bootstrap CDN"},
	{"fontawesome.com", "Font Awesome"},
	{"optimizely.com", "Optimizely"},
	{"vwo.com", "VWO"},
	{"crazyegg.com", "Crazy Egg"},
	{"fullstory.com", "FullStory"},
	{"logrocket.com", "LogRocket"},
	{"smartlook.com", "Smartlook"},
	{"mouseflow.com", "Mouseflow"},
	{"luckyorange.com", "Lucky Orange"},
	{"onetrust.com", "OneTrust"},
	{"cookiebot.com", "Cookiebot"},
	{"cookieyes.com", "CookieYes"},
	{"usercentrics.eu", "Usercentrics"},
	{"trustarc.com", "TrustArc"},
	{"mailchimp.com", "Mailchimp"},
	{"list-manage.com", "Mailchimp"},
	{"sendgrid.net", "SendGrid"},
	{"mandrillapp.com", "Mailchimp"},
	{"salesforce.com", "Salesforce"},
	{"force.com", "Salesforce"},
	{"pardot.com", "Pardot"},
	{"marketo.net", "Marketo"},
	{"mktoresp.com", "Marketo"},
	{"bing.com", "Microsoft Ads"},
	{"bat.bing.com", "Microsoft Ads"},
	{"adsrvr.org", "The Trade Desk"},
	{"criteo.com", "Criteo"},
	{"criteo.net", "Criteo"},
	{"taboola.com", "Taboola"},
	{"outbrain.com", "Outbrain"},
	{"adnxs.com", "AppNexus/Xandr"},
	{"rubiconproject.com", "Magnite"},
	{"pubmatic.com", "PubMatic"},
	{"casalemedia.com", "Index Exchange"},
	{"quantserve.com", "Quantcast"},
	{"scorecardresearch.com", "Comscore"},
	{"algolia.net", "Algolia"},
	{"algolianet.com", "Algolia"},
	{"pusher.com", "Pusher"},
	{"ably.io", "Ably"},
	{"auth0.com", "Auth0"},
	{"okta.com", "Okta"},
	{"recaptcha.com", "Google reCAPTCHA"},
}

var (
	trackerCacheMu sync.RWMutex
	trackerCache   = map[string]string{}
)

// trackerFor labels a host with a known tracker/vendor family, or "" when it is
// not in the catalogue. Results are cached because capture replays and repeated
// hosts are the common case.
func trackerFor(host string) string {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return ""
	}
	trackerCacheMu.RLock()
	label, ok := trackerCache[host]
	trackerCacheMu.RUnlock()
	if ok {
		return label
	}
	label = matchTracker(host)
	trackerCacheMu.Lock()
	if len(trackerCache) < 20000 {
		trackerCache[host] = label
	}
	trackerCacheMu.Unlock()
	return label
}

func matchTracker(host string) string {
	for _, entry := range trackerCatalogue {
		if strings.Contains(entry.suffix, "/") {
			// Path-qualified entries (e.g. facebook.com/tr) are handled by the
			// URL-aware variant; a bare host match would be too broad.
			continue
		}
		if host == entry.suffix || strings.HasSuffix(host, "."+entry.suffix) {
			return entry.label
		}
	}
	return ""
}

// trackerForURL additionally honours path-qualified catalogue entries, which is
// how pixel endpoints such as facebook.com/tr are recognised.
func trackerForURL(rawURL, host string) string {
	if label := trackerFor(host); label != "" {
		return label
	}
	lower := strings.ToLower(rawURL)
	for _, entry := range trackerCatalogue {
		slash := strings.IndexByte(entry.suffix, '/')
		if slash < 0 {
			continue
		}
		hostPart, pathPart := entry.suffix[:slash], entry.suffix[slash:]
		if (host == hostPart || strings.HasSuffix(host, "."+hostPart)) &&
			strings.Contains(lower, pathPart) {
			return entry.label
		}
	}
	return ""
}

// cookieFingerprints labels a cookie name by the product family that usually
// sets it. Values are never inspected — the name alone is a strong signal.
var cookieFingerprints = map[string]string{
	"_ga":                    "Google Analytics",
	"_gid":                   "Google Analytics",
	"_gat":                   "Google Analytics",
	"_gcl_au":                "Google Ads",
	"_fbp":                   "Meta Pixel",
	"_fbc":                   "Meta Pixel",
	"__cf_bm":                "Cloudflare",
	"cf_clearance":           "Cloudflare",
	"__cfduid":               "Cloudflare",
	"PHPSESSID":              "PHP",
	"JSESSIONID":             "Java",
	"laravel_session":        "Laravel",
	"XSRF-TOKEN":             "Laravel",
	"ci_session":             "CodeIgniter",
	"ASP.NET_SessionId":      "ASP.NET",
	".AspNetCore.Session":    "ASP.NET Core",
	"wp-settings-1":          "WordPress",
	"wordpress_logged_in":    "WordPress",
	"wp_woocommerce_session": "WooCommerce",
	"woocommerce_cart_hash":  "WooCommerce",
	"_shopify_s":             "Shopify",
	"shopify_cart_sig":       "Shopify",
	"intercom-session":       "Intercom",
	"__stripe_mid":           "Stripe",
	"__stripe_sid":           "Stripe",
	"_hjSessionUser":         "Hotjar",
	"_clck":                  "Microsoft Clarity",
	"_clsk":                  "Microsoft Clarity",
	"_uetsid":                "Microsoft Ads",
	"_uetvid":                "Microsoft Ads",
	"IDE":                    "Google Ads",
	"NID":                    "Google",
	"datr":                   "Meta",
	"fr":                     "Meta",
	"sb":                     "Meta",
}

// cookieFingerprint labels a cookie name, matching case-sensitively first and
// then case-insensitively (cookie names are technically case-sensitive, but
// vendors vary).
func cookieFingerprint(name string) string {
	if label, ok := cookieFingerprints[name]; ok {
		return label
	}
	for key, label := range cookieFingerprints {
		if strings.EqualFold(key, name) {
			return label
		}
	}
	return ""
}

// isBeaconType reports whether CDP reports this request as a beacon (sendBeacon
// and keepalive pings surface as resource type "Ping").
func isBeaconType(typ string) bool {
	return strings.EqualFold(typ, "Ping") || strings.EqualFold(typ, "Beacon")
}

// sortedKeys is a tiny helper for deterministic output in aggregates.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
