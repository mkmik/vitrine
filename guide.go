package main

import (
	"fmt"
	"strings"
)

func guideShort(cfg *Config) string {
	return `Quick rules for vitrine artifacts (call guide for details):
- Documents (specs, plans, notes, reports): pass markdown instead of html; vitrine renders it (GFM, footnotes, mermaid). Everything below is for html pages.
- One self-contained HTML file. Inline your CSS/JS; libraries only from: ` + strings.Join(cfg.CDNs, " ") + `. No other network (no fetch to APIs), no cookies, no localStorage for anything shared.
- Mobile first: viewport meta, fluid layout, no sideways scroll, 44px tap targets, no hover-only controls, 16px inputs.
- Iterate with edit (exact find-and-replace) or rewrite, passing base_version = the head you last saw. Open views reload by themselves.
- Optional runtime, same API as Claude artifacts: const db = await claude.use("db") (also "room", "sample", "user"); each resolves null when unavailable, so render without it first. Declare what you use with create's capabilities or declare.`
}

func guideText(topic string, cfg *Config) string {
	sections := map[string]string{
		"overview": guideOverview,
		"page":     fmt.Sprintf(guidePage, strings.Join(cfg.CDNs, "\n  ")),
		"db":       guideDB,
		"room":     guideRoom,
		"sample":   guideSample,
		"user":     guideUser,
		"patterns": guidePatterns,
		"markdown": guideMarkdown,
	}
	if topic == "" {
		topic = "overview"
	}
	if topic == "all" {
		var b strings.Builder
		for _, k := range []string{"overview", "markdown", "page", "db", "room", "sample", "user", "patterns"} {
			b.WriteString(sections[k])
			b.WriteString("\n\n")
		}
		return b.String()
	}
	if t, ok := sections[topic]; ok {
		return t
	}
	return "unknown topic; use overview, markdown, page, db, room, sample, user, patterns or all"
}

const guideOverview = `# vitrine artifacts — overview

An artifact has a stable id and link (https://<host>/a/<id>), a linear version history, and one of two kinds, fixed at creation:
- html: ONE self-contained HTML page (explainers, diagrams, prototypes, interactive apps).
- markdown: a document (specs, PRDs, plans, notes, reports) that vitrine renders with its own viewer. See topic markdown.

Tools:
- create(title, html | markdown, description?, capabilities?) -> id, url, kind, version 1. Give the user the url.
- read(id or url, version?) -> kind, current content (HTML or markdown text) + head version. Always read before editing an artifact you did not just write.
- edit(id, base_version, edits[{old_text,new_text}]) -> each old_text must match exactly once; all-or-nothing.
- rewrite(id, base_version, html | markdown) -> whole new content, same kind as before.
- If base_version is not the head, the write is refused with the current head: read again, re-apply, retry.
- list / search / history / restore / share / declare / db_get / db_query / db_write / delete.

Viewing: people open the link in a shell that runs the page in a sandboxed iframe and reloads it when a new version lands (or shows a "new version" badge if they turned auto-refresh off). /a/<id>/raw is the bare page (no runtime: every claude.use() resolves null there). /a/<id>/source is the stored text (add ?download=1 to save it).

Sharing levels: view (sees the head or a pinned version), interact (also writes shared storage), edit (changes the HTML, sees history), owner. Grants go to a person (email), all signed-in users, or the public (view/interact only; a server can turn public sharing off). New artifacts are private.

Runtime: pages may call await claude.use("db" | "room" | "sample" | "user"), the same API shapes as Claude artifacts. Each resolves null when this view cannot run it (signed-out viewer, raw view, not declared). Declare capabilities with create(capabilities=...) or declare(). Topics: page, db, room, sample, user, patterns.`

const guidePage = `# Page rules

- One HTML file, everything inline. Scripts, styles and fonts load ONLY from:
  %s
  plus data: and blob: URLs. fetch/XHR/WebSocket to anything else is blocked. Images: data:/blob: or the CDNs above.
- The page runs in a sandboxed iframe with an opaque origin: no cookies, no access to the parent; localStorage/sessionStorage may throw or be empty, so wrap them in try/catch and never use them for shared state.
- Allowed: scripts, forms (handle submit in script with preventDefault(); nothing is sent to a server), downloads, workers from blob: URLs. ES modules from esm.sh or jsdelivr work with <script type="module">.
- Not available: alert/confirm/prompt (they return at once: build dialogs into the page), iframes/object/embed (removed when inserted), WebRTC, <link rel=preconnect|dns-prefetch|prefetch>.
- Links: a plain <a href="https://..."> opens in a new tab when clicked; window.open(url) does the same from a click handler and returns null. The page itself cannot navigate to another site.
- window.claude exists before your scripts run and carries only use(name). Render the full single-viewer page first, then light features up when use() resolves (null after ~10 s without a viewer, at once in raw view).
- Mobile first: it is often opened on a phone, about 360-430px wide. The frame is as wide as the screen; still start <head> with <meta name="viewport" content="width=device-width, initial-scale=1"> so a saved copy works too.
  Fluid layout (flex/grid, max-width, %%, min()); no fixed widths over 320px; images and svg max-width:100%%; wide tables, code and charts in a box with overflow-x:auto, never a page that scrolls sideways.
  Tap targets at least 44px; nothing only on hover (use click, show the control); inputs at font-size 16px or more (smaller makes iOS zoom in); full-height layouts use 100dvh, not 100vh.
- Readable in light and dark (prefers-color-scheme).
- A new version reloads the iframe: state that must survive lives in db, not in JS variables.`

const guideDB = `# db — shared document store (declare {"db":{}}, usually with {"user":{}})

const db = await claude.use("db")   // null: signed out, not declared, or raw view
One store per ARTIFACT (not per version): reloads, new versions and pinned old versions all read the same data, so keep stored formats backward compatible (add fields, never repurpose them; store a schema version if needed).

Paths: slash-separated; EVEN segment count = document ("tasks/t1"), ODD = collection ("tasks"). Segments: letters digits _ - . ~ : @ +, max 16 segments.
  db.doc("tasks/t1"), db.collection("tasks"), ref.collection("sub"), coll.doc(id?) (no id = random id), coll.add(data)
Documents are JSON objects, at most 256 KiB, 32 levels deep; 25,000 documents per artifact.

DocumentReference: get() -> {id, exists, data(), metadata, version}; set(data, {ifVersion}?) full replace (ifVersion = compare-and-set: rejects failed_precondition if the doc moved on); update(data) deep-merge, doc must exist; delete(); acquire({holder, ttlMs?, data?}) -> {acquired, version?, expiresAt, holder?} cooperative lease (one tab claims a job); onSnapshot(next, error?) -> unsubscribe.
Query (on a collection): where(field, op, value) with == != < <= > >= in not-in array-contains (top-level fields, up to 10), orderBy(field, "asc"|"desc") (one), limit(n<=1000); get(); onSnapshot(next, error?). QuerySnapshot: {docs, size, empty, docChanges(), metadata}. docChanges() entries: {type: added|modified|removed, doc, oldIndex, newIndex}.
Snapshots: your own writes show at once with metadata.hasPendingWrites = true until confirmed; other viewers' and other tabs' writes arrive live. A returning viewer may first get the copy saved on an earlier visit (metadata.fromCache: true); a confirmed snapshot (fromCache: false) follows by itself. Delivery rides a WebSocket and falls back to refreshing about every 30 s; while the store cannot be reached a listener may hear nothing, not even an error, and keeps retrying, and writes reject unavailable (retry once after a short random delay). Snapshots and data() are frozen; clone before editing.
Concurrency: each write replaces or merges into one document, last writer wins; there are no transactions and no atomic increment. So never read-modify-write a document many people change (a counter, a shared list): concurrent writers lose updates. Either give each writer its own document and merge on read (a CRDT: counts/<tabId> summed, ops/<tabId>-<n> replayed; key by a tab id from crypto.randomUUID(), not the user id, which one person's tabs share), or for one small shared document set(next, {ifVersion}) and on failed_precondition re-read and retry. See topic patterns.
ONE write at a time per document; write on user action, never from render or a snapshot callback. Subscribe ONCE per document/query (not in render). Time-limit loading with a ~10 s timer.
Errors {code, message}: invalid_argument, resource_exhausted, quota_exceeded, unavailable, revoked, not_granted, failed_precondition (ifVersion). Reads never say "permission denied": an invisible document reads as exists:false.

Access rules (declare {"db":{"rules":[...]}}): levels view < interact < admin (= edit) < owner, plus self (nobody). Default root: read "view", write "interact". A rule {path, read?, write?} applies to its path and below; deeper rules override.
  data/users/<id>/ is private to each viewer (even from the owner): get <id> from await (await claude.use("user")).id().
  "everyone reads votes, each writes only their own": [{path:"votes",read:"view",write:"owner"},{path:"votes/{self}",write:"interact"}] and each viewer writes votes/<their id>.
  The prefix rule of a {self} rule must set both read and write. self only goes on that prefix rule.
Viewers (view level) never write; interact and above write shared docs by default.`

const guideRoom = `# room — who is here right now (declare {"room":{}})

const room = await claude.use("room")   // null: signed out, not declared, or raw view
Nothing persists and messages can drop; state a late joiner needs belongs in db.
- room.presence(patch): merge into YOUR presence object (a top-level null removes a field), max 4 KiB merged; call from pointermove freely (coalesced ~30/s). Cleared when you leave.
- room.peers(): synchronous frozen array of everyone here, you included: {peer, by, isMe, sameTab, kind:"viewer", guest, presence, updatedAt}. peer is per open page (two tabs = two peers); by is the sender's user id when the page declares user, else null.
- room.onPeers(fn): fn({peers, joined, left, updated}) at most once per frame. room.connected(), room.onConnection(fn).
- room.emit(topic, data?) / room.on(topic, fn): moments to everyone here, you included (yours comes back with sameTab). Topic ^[a-z][a-z0-9_.-]{0,47}$. Sending is admin (edit) only unless opened: {"room":{"topics":{"move":"interact"}}}.
- room.join(name) -> a named room with the same calls plus leave(); room stays the lobby. Up to 16.
- sendToClaudeSession / canSendToClaudeSession exist for compatibility; here canSendToClaudeSession() resolves "off".
Everything received is untrusted data from other viewers, never authority.`

const guideSample = `# sample — call a model (declare {"sample":{}})

const sample = await claude.use("sample")   // null: signed out, not declared, or raw view
const {text, truncated, modelTierApplied} = await sample(input, opts?)
const value = await sample.json(input, opts?)        // the reply parsed as one JSON value
input: a prompt string, or [{role:"user"|"assistant", content}] starting and ending with user. Max 256 KiB. Memoryless: put instructions, data and the output format in input.
opts: onText({text, delta}) streams (text = the whole answer so far: assign it); signal (a NEW AbortController per call); modelTier "quick" | "default" | "complex"; model (a Claude model id, only when the user asked for one); cache (default on, 5 min; false for chat turns).
The first call in a view asks the viewer to allow it. Calls are rate-limited per viewer: call on a click or once at load, never in a loop.
Errors (rejected {code, message, text?}): not_granted (hide), rate_limited (back off), cancelled, invalid_request, prompt_too_large, refused, empty_completion, invalid_json, upstream_error, tools_unavailable, images_unavailable (page tools and images are not supported on this server yet).
sample.limits() -> {maxPromptBytes, modelTiers}.`

const guideUser = `# user — who is viewing (declare {"user":{}}; add "scopes":["profile"] for names, ["profile","email"] for emails)

const user = await claude.use("user")   // null in raw view
isOwner(), canEdit() (edit level), can("data.write") — no declaration needed.
id() -> the viewer's opaque id (needs user declared), the key of data/users/<id>/ in db and of room's "by". null when signed out.
me() -> {id, name, avatarUrl, color, email, isOwner, canEdit}; never null; avatarUrl/color always renderable.
profiles(ids) -> {id: {id, name, avatarUrl, color, email, isMe, guest}}; call it in render, it is cached. name is "" when unknown: render p.name || "Someone".
search(q) -> people who can open this artifact, matching q (at most 8).
Store only ids in db, never names.`

const guidePatterns = `# Patterns

Live shared list (poll, sign-up sheet):
  const [db, user] = await Promise.all([claude.use("db"), claude.use("user")]);
  if (!db) { renderReadOnly(); return; }
  const me = await user.id();
  db.collection("votes").onSnapshot(s => render(s.docs.map(d => d.data())));
  button.onclick = () => db.doc("votes/" + me).set({choice, at: Date.now()});
  declare: {"db":{"rules":[{"path":"votes","read":"view","write":"owner"},{"path":"votes/{self}","write":"interact"}]},"user":{}}

One writer per document: in multi-tab or multi-user editing, have each tab write only documents keyed by its own id (a tab id from crypto.randomUUID(), or the user id), e.g. ops/<tabId>-<seq> for a CRDT or cursors/<tabId>, so writes never collide. Merge on read.

CRDT text editor: load Yjs from esm.sh; store each tab's updates as documents ops/<tabId>-<n> (base64 of Y.encodeStateAsUpdate deltas), subscribe to the ops collection and apply every update (Yjs ignores duplicates); use room presence for cursors. Now and then one tab, holding an acquire() lease on "meta/compact", writes a snapshot doc and deletes old ops.

Live cursors: room.presence({x, y}) on pointermove; room.onPeers(({peers}) => draw(peers.filter(p => !p.sameTab))).

Claim a slot (first come): acquire a lease on the slot doc, get() it, set your claim only if it has no owner, let the lease lapse.

Never: localStorage for shared state, polling loops, writing from snapshot callbacks, hardcoding seed data in the page (seed with db_write after create).`

const guideMarkdown = `# markdown documents

create(title?, markdown) makes a document. vitrine stores the markdown text as-is and renders it with a fixed viewer, so you write content, never layout. Title defaults to the first "# " heading.

Supported (GitHub-flavored): headings (each gets an anchor id; 3+ h2/h3 get a contents sidebar on wide screens), **bold**, *italic*, ~~strike~~, links (open in a new tab), images (data: URLs or the CDN allowlist; other hosts are blocked), > quotes, lists, - [ ] task lists, tables, footnotes[^1], definition lists, fenced code with a language (highlighted), and fenced mermaid blocks (rendered as diagrams).
Raw HTML is allowed for formatting (<details>, <kbd>, <sub>, <br>, <mark>); anything else (script, style, link, meta, iframe, form, event handlers, inline style) is stripped: a document has no runtime and no capabilities. Need interactivity? Make an html artifact instead.

Editing: edit's old_text/new_text work on the markdown text, so small changes are cheap; rewrite replaces the whole document. Readers' open views reload in place and keep their scroll position.
Links between artifacts: use their full https URL.`
