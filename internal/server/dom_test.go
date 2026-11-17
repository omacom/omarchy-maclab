package server

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

// A page read as a tree, and a small part of CSS's selectors over it, for
// the tests translated from the old site's assert_select and css_select:
// tag, .class, #id, [attr], [attr=v], [attr^=v], [attr~=v], compounds of
// those, and the descendant space.

type node struct {
	tag      string
	attrs    map[string]string
	has      map[string]bool
	parent   *node
	children []*node
	chars    string // a text node's text
}

var (
	reTagOpen = regexp.MustCompile(`^<([a-zA-Z][a-zA-Z0-9]*)((?:\s+[^\s=>/]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?)*)\s*/?>`)
	reAttr    = regexp.MustCompile(`([^\s=>/]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?`)
	voidTags  = map[string]bool{"meta": true, "link": true, "img": true, "input": true, "br": true, "hr": true}
)

func parseHTML(t testing.TB, text string) *node {
	t.Helper()
	root := &node{tag: "#root"}
	at := root
	for len(text) > 0 {
		switch {
		case strings.HasPrefix(text, "<!"):
			end := strings.Index(text, ">")
			text = text[end+1:]
		case strings.HasPrefix(text, "</"):
			end := strings.Index(text, ">")
			name := strings.ToLower(strings.TrimSpace(text[2:end]))
			for n := at; n != nil; n = n.parent {
				if n.tag == name {
					at = n.parent
					break
				}
			}
			text = text[end+1:]
		case reTagOpen.MatchString(text):
			m := reTagOpen.FindStringSubmatch(text)
			n := &node{tag: strings.ToLower(m[1]), attrs: map[string]string{}, has: map[string]bool{}, parent: at}
			for _, a := range reAttr.FindAllStringSubmatch(m[2], -1) {
				n.attrs[a[1]], n.has[a[1]] = html.UnescapeString(a[2]+a[3]+a[4]), true
			}
			at.children = append(at.children, n)
			text = text[len(m[0]):]
			if n.tag == "script" || n.tag == "style" {
				end := strings.Index(text, "</"+n.tag)
				text = text[end:]
			}
			if !voidTags[n.tag] {
				at = n
			}
		default:
			end := strings.Index(text[1:], "<") + 1
			if end == 0 {
				end = len(text)
			}
			at.children = append(at.children, &node{chars: html.UnescapeString(text[:end]), parent: at})
			text = text[end:]
		}
	}
	return root
}

// text is the node's text, as a reader sees it: what is hidden, such as a
// Copy button before the script shows it, is not read.
func (n *node) text() string {
	if n.tag == "" {
		return n.chars
	}
	if n.has["hidden"] {
		return ""
	}
	var b strings.Builder
	for _, c := range n.children {
		b.WriteString(c.text())
	}
	return b.String()
}

// words is text with its white space folded.
func (n *node) words() string { return strings.Join(strings.Fields(n.text()), " ") }

func (n *node) attr(name string) string { return n.attrs[name] }

func (n *node) classes() []string { return strings.Fields(n.attrs["class"]) }

type simple struct {
	tag     string
	classes []string
	id      string
	attrs   []attrTest
}

type attrTest struct{ name, op, value string }

var reSimplePart = regexp.MustCompile(`^(?:([a-zA-Z][a-zA-Z0-9]*)|\.([\w-]+)|#([\w.-]+)|\[([\w-]+)(?:([~^]?=)"?([^\]"]*)"?)?\])`)

func parseSelector(t testing.TB, selector string) []simple {
	t.Helper()
	var chain []simple
	for _, part := range splitSelector(selector) {
		var s simple
		for part != "" {
			m := reSimplePart.FindStringSubmatch(part)
			if m == nil {
				t.Fatalf("selector %q: cannot read %q", selector, part)
			}
			switch {
			case m[1] != "":
				s.tag = strings.ToLower(m[1])
			case m[2] != "":
				s.classes = append(s.classes, m[2])
			case m[3] != "":
				s.id = m[3]
			default:
				s.attrs = append(s.attrs, attrTest{m[4], m[5], m[6]})
			}
			part = part[len(m[0]):]
		}
		chain = append(chain, s)
	}
	return chain
}

// splitSelector splits at spaces outside brackets.
func splitSelector(selector string) []string {
	var parts []string
	depth, start := 0, 0
	for i, c := range selector + " " {
		switch {
		case c == '[':
			depth++
		case c == ']':
			depth--
		case c == ' ' && depth == 0:
			if i > start {
				parts = append(parts, selector[start:i])
			}
			start = i + 1
		}
	}
	return parts
}

func (s simple) matches(n *node) bool {
	if n.tag == "" || (s.tag != "" && s.tag != n.tag) || (s.id != "" && s.id != n.attrs["id"]) {
		return false
	}
	for _, class := range s.classes {
		found := false
		for _, c := range n.classes() {
			found = found || c == class
		}
		if !found {
			return false
		}
	}
	for _, a := range s.attrs {
		value := n.attrs[a.name]
		switch {
		case !n.has[a.name]:
			return false
		case a.op == "=" && value != a.value:
			return false
		case a.op == "^=" && !strings.HasPrefix(value, a.value):
			return false
		case a.op == "~=" && !strings.Contains(" "+value+" ", " "+a.value+" "):
			return false
		}
	}
	return true
}

// all are the nodes under n the selector picks, in the page's order.
func (n *node) all(t testing.TB, selector string) []*node {
	t.Helper()
	chain := parseSelector(t, selector)
	var found []*node
	var walk func(*node)
	walk = func(at *node) {
		for _, c := range at.children {
			if chain[len(chain)-1].matches(c) && under(c, chain[:len(chain)-1], n) {
				found = append(found, c)
			}
			walk(c)
		}
	}
	walk(n)
	return found
}

func under(c *node, chain []simple, top *node) bool {
	if len(chain) == 0 {
		return true
	}
	for p := c.parent; p != nil && p != top.parent; p = p.parent {
		if chain[len(chain)-1].matches(p) && under(p, chain[:len(chain)-1], top) {
			return true
		}
	}
	return false
}

// sole is the one node the selector picks.
func (n *node) sole(t testing.TB, selector string) *node {
	t.Helper()
	found := n.all(t, selector)
	if len(found) != 1 {
		t.Fatalf("%d nodes match %s, want 1", len(found), selector)
	}
	return found[0]
}

func (n *node) count(t testing.TB, selector string) int {
	t.Helper()
	return len(n.all(t, selector))
}

// has reports whether some node the selector picks has these words; "" asks only for a node.
func (n *node) hasText(t testing.TB, selector, words string) bool {
	t.Helper()
	for _, found := range n.all(t, selector) {
		if words == "" || found.words() == words {
			return true
		}
	}
	return false
}

// says reports whether some node the selector picks contains these words.
func (n *node) says(t testing.TB, selector, part string) bool {
	t.Helper()
	for _, found := range n.all(t, selector) {
		if strings.Contains(found.words(), part) {
			return true
		}
	}
	return false
}

func attrsOf(nodes []*node, name string) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.attrs[name])
	}
	return out
}
