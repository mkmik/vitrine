package main

import (
	"bytes"
	"html"
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	ghtml "github.com/yuin/goldmark/renderer/html"
)

// Artifact kinds. An html artifact is a page; a markdown artifact is a
// document shown by vitrine's own fixed renderer.
const (
	kindHTML     = "html"
	kindMarkdown = "markdown"
)

func validKind(k string) bool { return k == kindHTML || k == kindMarkdown }

// Raw HTML inside markdown is rendered, then everything goes through an
// allowlist sanitizer: formatting tags (details, kbd, sub, mark...) survive;
// script, style, link, meta, iframe, form and event handlers do not. The page
// CSP (renderer's nonce only) is a second wall behind it.
var mdEngine = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Footnote, extension.DefinitionList),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(ghtml.WithUnsafe()),
)

var mdPolicy = sync.OnceValue(func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowElements("details", "summary", "kbd", "mark", "ins", "del", "s", "abbr", "figure", "figcaption")
	p.AllowAttrs("open").OnElements("details")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^[\w\- ]{1,100}$`)).Globally()
	p.AllowAttrs("role").Matching(regexp.MustCompile(`^doc-[a-z]+$`)).Globally()
	p.AllowAttrs("type").Matching(regexp.MustCompile(`^checkbox$`)).OnElements("input")
	p.AllowAttrs("checked", "disabled").OnElements("input")
	p.AllowStyles("text-align").MatchingEnum("left", "right", "center").OnElements("th", "td")
	p.AllowAttrs("align").Matching(regexp.MustCompile(`^(left|right|center)$`)).OnElements("th", "td")
	p.AllowDataURIImages()
	return p
})

func renderMarkdown(src string) (string, error) {
	var b bytes.Buffer
	if err := mdEngine.Convert([]byte(src), &b); err != nil {
		return "", err
	}
	return mdPolicy().Sanitize(b.String()), nil
}

var mdAssets = sync.OnceValues(func() (string, string) {
	css, _ := webFS.ReadFile("web/md.css")
	js, _ := webFS.ReadFile("web/md.js")
	return string(css), strings.ReplaceAll(string(js), "</script", "<\\/script")
})

// markdownPage wraps rendered markdown in vitrine's document page.
func markdownPage(title, src, nonce string) string {
	body, err := renderMarkdown(src)
	if err != nil {
		body = "<pre>" + html.EscapeString(src) + "</pre>"
	}
	css, js := mdAssets()
	var b strings.Builder
	b.Grow(len(body) + len(css) + len(js) + 512)
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>`)
	b.WriteString(html.EscapeString(title))
	b.WriteString(`</title><style>`)
	b.WriteString(css)
	b.WriteString(`</style></head><body><nav id="toc" hidden></nav><main class="md" id="doc">`)
	b.WriteString(body)
	b.WriteString(`</main><script nonce="`)
	b.WriteString(nonce)
	b.WriteString(`">`)
	b.WriteString(js)
	b.WriteString(`</script></body></html>`)
	return b.String()
}

// markdownCSP is the content CSP with scripts limited to the renderer
// (nonce + strict-dynamic, so it may load pinned mermaid / highlight.js).
// Images come only from data: URLs and the CDN allowlist: a remote image is
// a way to leak data to whoever hosts it.
func (s *Server) markdownCSP(nonce, frameAncestors string) string {
	cdn := strings.Join(s.cfg.CDNs, " ")
	return strings.Join([]string{
		"sandbox allow-scripts",
		"default-src 'none'",
		"script-src 'nonce-" + nonce + "' 'strict-dynamic'",
		"style-src 'unsafe-inline' " + cdn,
		"font-src data: " + cdn,
		"img-src data: blob: " + cdn,
		"media-src data: blob:",
		"connect-src 'none'",
		"frame-src 'none'",
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors " + frameAncestors,
	}, "; ")
}
