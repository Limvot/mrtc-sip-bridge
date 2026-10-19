package widget

import (
	"regexp"
	"testing"
)

// Every element a client looks up by a literal id must exist in its index.html (the two hand-written
// frontends have no build step or type checker), including the Contacts tab ids.
func TestStaticIDsDefined(t *testing.T) {
	use := regexp.MustCompile(`\$\('([A-Za-z][\w-]*)'\)`)
	def := regexp.MustCompile(`\bid="([^"]+)"`)
	for _, dir := range []string{"static", "static/app"} {
		html, err := staticFS.ReadFile(dir + "/index.html")
		if err != nil {
			t.Fatal(err)
		}
		js, err := staticFS.ReadFile(dir + "/app.js")
		if err != nil {
			t.Fatal(err)
		}
		have := map[string]bool{}
		for _, m := range def.FindAllStringSubmatch(string(html), -1) {
			if have[m[1]] {
				t.Errorf("%s/index.html: duplicate id %q", dir, m[1])
			}
			have[m[1]] = true
		}
		for _, m := range use.FindAllStringSubmatch(string(js), -1) {
			if !have[m[1]] {
				t.Errorf("%s/app.js uses $('%s') but index.html has no such id", dir, m[1])
			}
		}
		for _, id := range []string{"tabs", "tab-dial", "tab-contacts", "panel-dial", "panel-contacts", "contact-search",
			"contacts-status", "contacts-list", "contacts-more", "contacts-retry"} {
			if !have[id] {
				t.Errorf("%s/index.html lacks %q", dir, id)
			}
		}
	}
}
