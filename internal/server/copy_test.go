package server

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

// reCopy is a value a page offers to copy, marked data-copy, and the Copy
// button beside it in the same .copyable, rendered hidden for app.js to show.
var reCopy = regexp.MustCompile(`<(code|h1) data-copy(?: id="[^"]*")?>([^<]*)</(?:code|h1)>(?:</pre>)? <button class="button button-small button-plain copy-button" type="button" aria-label="Copy ([^"]*)" hidden>Copy</button>`)

// copies are the values a page offers to copy, by what each one's button
// names. A target with no button beside it, or a button with no target, is
// an error.
func copies(t *testing.T, page string) map[string]string {
	t.Helper()
	found := map[string]string{}
	ms := reCopy.FindAllStringSubmatch(page, -1)
	for _, m := range ms {
		found[html.UnescapeString(m[3])] = html.UnescapeString(m[2])
	}
	if targets, buttons := strings.Count(page, " data-copy"), strings.Count(page, "copy-button\""); targets != len(ms) || buttons != len(ms) {
		t.Errorf("the page has %d values to copy and %d Copy buttons, %d of them side by side", targets, buttons, len(ms))
	}
	return found
}

// offersToCopy fails t unless page has a Copy button naming what beside text.
func offersToCopy(t *testing.T, page, what, text string) {
	t.Helper()
	if got, ok := copies(t, page)[what]; !ok || got != text {
		t.Errorf("the Copy button for %s copies %q (there: %v), want %q", what, got, ok, text)
	}
}
