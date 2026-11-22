package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Hub is the in-process live layer: head changes, db snapshots and rooms.
type Hub struct {
	s     *Server
	mu    sync.Mutex
	conns map[string]map[*wsConn]struct{}        // artifact -> connections
	rooms map[string]map[string]map[*wsConn]bool // artifact -> room name -> members
}

func newHub(s *Server) *Hub {
	return &Hub{s: s, conns: map[string]map[*wsConn]struct{}{}, rooms: map[string]map[string]map[*wsConn]bool{}}
}

type dbSub struct {
	id   string
	path string
	q    *Query
	last [32]byte
	mu   sync.Mutex // serializes evaluations so snapshots stay in order
}

type wsConn struct {
	h    *Hub
	aid  string
	user *User
	send chan []byte
	done chan struct{}
	once sync.Once

	mu       sync.Mutex
	peer     string
	acc      Access
	caps     *Caps
	subs     map[string]*dbSub
	presence map[string]map[string]any // room -> presence object
	updated  map[string]int64
	rooms    map[string]bool
}

func (c *wsConn) close() { c.once.Do(func() { close(c.done) }) }

func (c *wsConn) pushRaw(b []byte) {
	select {
	case c.send <- b:
	case <-c.done:
	default:
		log.Printf("ws: slow consumer on %s, closing", c.aid)
		c.close()
	}
}

func (c *wsConn) push(v any) {
	b, err := json.Marshal(v)
	if err == nil {
		c.pushRaw(b)
	}
}

func (c *wsConn) viewer() (Viewer, *Caps) {
	c.mu.Lock()
	defer c.mu.Unlock()
	uid := ""
	if c.user != nil {
		uid = c.user.ID
	}
	return Viewer{UID: uid, Level: c.acc.Level}, c.caps
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.artifact(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.sameOrigin(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	u := s.userOf(r)
	acc := s.st.accessFor(a, u)
	if acc.Level == lvNone {
		http.NotFound(w, r)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{s.base.Host}})
	if err != nil {
		return
	}
	ws.SetReadLimit(2 << 20)
	c := &wsConn{h: s.hub, aid: a.ID, user: u, send: make(chan []byte, 1024), done: make(chan struct{}),
		peer: randID(10), acc: acc, caps: a.Caps(), subs: map[string]*dbSub{}, presence: map[string]map[string]any{},
		updated: map[string]int64{}, rooms: map[string]bool{}}
	s.hub.add(c)
	defer s.hub.remove(c)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	uid := ""
	if u != nil {
		uid = u.ID
	}
	c.push(map[string]any{"ev": "hello", "peer": c.peer, "level": levelName(acc.Level), "pinned": acc.Pinned, "head": a.Head,
		"title": a.Title, "caps": a.Caps().Names, "capabilities": a.Capabilities, "uid": uid})

	go func() { // writer
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case b := <-c.send:
				wctx, wc := context.WithTimeout(ctx, 10*time.Second)
				err := ws.Write(wctx, websocket.MessageText, b)
				wc()
				if err != nil {
					c.close()
					return
				}
			case <-t.C:
				pctx, pc := context.WithTimeout(ctx, 10*time.Second)
				ws.Ping(pctx)
				pc()
			case <-c.done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		select {
		case <-c.done:
		case <-ctx.Done():
		}
		ws.Close(websocket.StatusNormalClosure, "")
		cancel()
	}()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			c.close()
			return
		}
		var m wsMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		c.handle(&m)
	}
}

type wsMsg struct {
	ID     int             `json:"id"`
	Op     string          `json:"op"`
	Path   string          `json:"path,omitempty"`
	Q      *Query          `json:"q,omitempty"`
	W      *WriteOp        `json:"w,omitempty"`
	Sub    string          `json:"sub,omitempty"`
	Holder string          `json:"holder,omitempty"`
	TTL    int             `json:"ttlMs,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Room   string          `json:"room"`
	Topic  string          `json:"topic,omitempty"`
	Patch  map[string]any  `json:"patch,omitempty"`
	IDs    []string        `json:"ids,omitempty"`
	Query  string          `json:"query,omitempty"`
}

func (c *wsConn) reply(id int, result any, e *DBError) {
	if id == 0 {
		return
	}
	if e != nil {
		c.push(map[string]any{"re": id, "error": e})
		return
	}
	c.push(map[string]any{"re": id, "ok": true, "result": result})
}

var roomNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,47}$`)

func (c *wsConn) handle(m *wsMsg) {
	s := c.h.s
	v, caps := c.viewer()
	c.mu.Lock()
	acc := c.acc
	c.mu.Unlock()
	signedIn := c.user != nil
	if m.Op == "ping" {
		c.reply(m.ID, "pong", nil)
		return
	}
	if strings.HasPrefix(m.Op, "db.") {
		if caps.DB == nil || !signedIn {
			c.reply(m.ID, nil, dbErr("not_granted", "db is not available in this view"))
			return
		}
		if !s.allow("db:"+c.peer, 50, 200) {
			c.reply(m.ID, nil, dbErr("resource_exhausted", "too many calls"))
			return
		}
	}
	if strings.HasPrefix(m.Op, "room.") {
		if caps.Room == nil || !signedIn {
			c.reply(m.ID, nil, dbErr("not_granted", "room is not available in this view"))
			return
		}
		if m.Room != "" && !roomNameRE.MatchString(m.Room) {
			c.reply(m.ID, nil, dbErr("invalid_argument", "bad room name"))
			return
		}
	}
	switch m.Op {
	case "db.get":
		d, err := s.st.docGet(c.aid, caps, v, m.Path)
		c.reply(m.ID, d, err)
	case "db.query":
		if m.Q == nil {
			c.reply(m.ID, nil, dbErr("invalid_argument", "missing query"))
			return
		}
		docs, err := s.st.docQuery(c.aid, caps, v, *m.Q)
		if err != nil {
			c.reply(m.ID, nil, err)
			return
		}
		c.reply(m.ID, map[string]any{"docs": docs}, nil)
	case "db.write":
		if m.W == nil {
			c.reply(m.ID, nil, dbErr("invalid_argument", "missing write"))
			return
		}
		res, err := s.st.docWrite(c.aid, caps, v, *m.W, s.cfg.MaxDocBytes, s.cfg.MaxDocs)
		if err != nil {
			c.reply(m.ID, nil, err)
			return
		}
		c.reply(m.ID, res, nil)
		c.h.docChanged(c.aid, m.W.Path)
	case "db.acquire":
		var data map[string]any
		if len(m.Data) > 0 && string(m.Data) != "null" {
			if json.Unmarshal(m.Data, &data) != nil {
				c.reply(m.ID, nil, dbErr("invalid_argument", "data must be an object"))
				return
			}
		}
		res, err := s.st.docAcquire(c.aid, caps, v, m.Path, m.Holder, m.TTL, data, s.cfg.MaxDocBytes)
		if err != nil {
			c.reply(m.ID, nil, err)
			return
		}
		c.reply(m.ID, res, nil)
		if res.Acquired {
			c.h.docChanged(c.aid, m.Path)
		}
	case "db.sub":
		if (m.Q == nil) == (m.Path == "") {
			c.reply(m.ID, nil, dbErr("invalid_argument", "subscribe to a path or a query"))
			return
		}
		c.mu.Lock()
		if len(c.subs) >= 64 {
			c.mu.Unlock()
			c.reply(m.ID, nil, dbErr("resource_exhausted", "at most 64 subscriptions per view"))
			return
		}
		sub := &dbSub{id: m.Sub, path: m.Path, q: m.Q}
		c.subs[m.Sub] = sub
		c.mu.Unlock()
		if err := c.evalSub(sub, true); err != nil {
			c.mu.Lock()
			delete(c.subs, m.Sub)
			c.mu.Unlock()
			c.reply(m.ID, nil, err)
			return
		}
		c.reply(m.ID, true, nil)
	case "db.unsub":
		c.mu.Lock()
		delete(c.subs, m.Sub)
		c.mu.Unlock()
		c.reply(m.ID, true, nil)
	case "frame.reset":
		// the shell loaded a new document: drop its subscriptions and rooms, new peer label
		c.mu.Lock()
		c.subs = map[string]*dbSub{}
		var rooms []string
		for r := range c.rooms {
			rooms = append(rooms, r)
		}
		c.mu.Unlock()
		for _, r := range rooms {
			c.h.leaveRoom(c, r)
		}
		c.mu.Lock()
		c.peer = randID(10)
		c.presence = map[string]map[string]any{}
		peer := c.peer
		c.mu.Unlock()
		c.reply(m.ID, map[string]string{"peer": peer}, nil)
	case "room.join":
		c.mu.Lock()
		n, in := len(c.rooms), c.rooms[m.Room]
		c.mu.Unlock()
		if !in && n >= 17 {
			c.reply(m.ID, nil, dbErr("limit_reached", "at most 16 named rooms"))
			return
		}
		c.h.joinRoom(c, m.Room)
		c.reply(m.ID, true, nil)
	case "room.leave":
		c.h.leaveRoom(c, m.Room)
		c.reply(m.ID, true, nil)
	case "room.presence":
		if !s.allow("room:"+c.peer, 40, 80) {
			c.reply(m.ID, nil, dbErr("upstream_error", "rate limited"))
			return
		}
		c.mu.Lock()
		if !c.rooms[m.Room] {
			c.mu.Unlock()
			c.reply(m.ID, nil, dbErr("invalid_argument", "not in that room"))
			return
		}
		next := map[string]any{}
		for k, x := range c.presence[m.Room] {
			next[k] = x
		}
		for k, x := range m.Patch {
			if x == nil {
				delete(next, k)
			} else {
				next[k] = x
			}
		}
		if b, _ := json.Marshal(next); len(b) > 4096 {
			c.mu.Unlock()
			c.reply(m.ID, nil, dbErr("invalid_argument", "presence over 4 KiB"))
			return
		}
		c.presence[m.Room] = next
		c.updated[m.Room] = time.Now().UnixMilli()
		c.mu.Unlock()
		c.reply(m.ID, true, nil)
		c.h.broadcastPeers(c.aid, m.Room)
	case "room.emit":
		if !topicRE.MatchString(m.Topic) {
			c.reply(m.ID, nil, dbErr("invalid_argument", "bad topic"))
			return
		}
		if len(m.Data) > 4096 {
			c.reply(m.ID, nil, dbErr("invalid_argument", "data over 4 KiB"))
			return
		}
		need := lvEdit
		if caps.Room != nil {
			if lv, ok := caps.Room.Topics[m.Topic]; ok {
				need = levelNames[lv]
			}
		}
		if acc.Level < need {
			c.reply(m.ID, nil, dbErr("not_permitted", "this viewer may not send on topic %q", m.Topic))
			return
		}
		c.mu.Lock()
		in := c.rooms[m.Room]
		c.mu.Unlock()
		if !in {
			c.reply(m.ID, nil, dbErr("invalid_argument", "not in that room"))
			return
		}
		c.reply(m.ID, true, nil)
		if s.allow("room:"+c.peer, 40, 80) {
			c.h.broadcastMsg(c, m.Room, m.Topic, m.Data)
		}
	case "user.profiles":
		c.reply(m.ID, c.h.profiles(caps, m.IDs), nil)
	case "user.search":
		c.reply(m.ID, c.h.search(c, caps, m.Query), nil)
	default:
		c.reply(m.ID, nil, dbErr("invalid_argument", "unknown op %q", m.Op))
	}
}

// evalSub runs a subscription's read and pushes a snapshot if it changed.
func (c *wsConn) evalSub(sub *dbSub, force bool) *DBError {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	s := c.h.s
	v, caps := c.viewer()
	var payload map[string]any
	if sub.q != nil {
		docs, err := s.st.docQuery(c.aid, caps, v, *sub.q)
		if err != nil {
			return err
		}
		payload = map[string]any{"ev": "db.snap", "sub": sub.id, "docs": docs}
	} else {
		d, err := s.st.docGet(c.aid, caps, v, sub.path)
		if err != nil {
			return err
		}
		payload = map[string]any{"ev": "db.snap", "sub": sub.id, "doc": d}
	}
	b, _ := json.Marshal(payload)
	h := sha256.Sum256(b)
	if !force && h == sub.last {
		return nil
	}
	sub.last = h
	c.pushRaw(b)
	return nil
}

func (h *Hub) add(c *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.aid] == nil {
		h.conns[c.aid] = map[*wsConn]struct{}{}
	}
	h.conns[c.aid][c] = struct{}{}
}

func (h *Hub) remove(c *wsConn) {
	c.mu.Lock()
	var rooms []string
	for r := range c.rooms {
		rooms = append(rooms, r)
	}
	c.mu.Unlock()
	for _, r := range rooms {
		h.leaveRoom(c, r)
	}
	h.mu.Lock()
	delete(h.conns[c.aid], c)
	if len(h.conns[c.aid]) == 0 {
		delete(h.conns, c.aid)
	}
	h.mu.Unlock()
}

func (h *Hub) connsOf(aid string) []*wsConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*wsConn, 0, len(h.conns[aid]))
	for c := range h.conns[aid] {
		out = append(out, c)
	}
	return out
}

func (h *Hub) artifactChanged(a *Artifact) {
	for _, c := range h.connsOf(a.ID) {
		c.mu.Lock()
		c.caps = a.Caps()
		c.mu.Unlock()
		c.push(map[string]any{"ev": "head", "head": a.Head, "title": a.Title, "caps": a.Caps().Names, "capabilities": a.Capabilities})
	}
}

func (h *Hub) artifactDeleted(id string) {
	for _, c := range h.connsOf(id) {
		c.push(map[string]any{"ev": "deleted"})
	}
}

func (h *Hub) accessChanged(id string) {
	a, err := h.s.st.artifact(id)
	if err != nil {
		return
	}
	for _, c := range h.connsOf(id) {
		acc := h.s.st.accessFor(a, c.user)
		c.mu.Lock()
		old := c.acc
		c.acc = acc
		c.mu.Unlock()
		if acc.Level != old.Level || acc.Pinned != old.Pinned {
			c.push(map[string]any{"ev": "access", "level": levelName(acc.Level), "pinned": acc.Pinned})
			if acc.Level == lvNone {
				c.close()
			}
		}
	}
}

func (h *Hub) docChanged(aid, path string) {
	coll := collOf(path)
	for _, c := range h.connsOf(aid) {
		c.mu.Lock()
		var hit []*dbSub
		for _, sub := range c.subs {
			if (sub.q == nil && sub.path == path) || (sub.q != nil && sub.q.Coll == coll) {
				hit = append(hit, sub)
			}
		}
		c.mu.Unlock()
		for _, sub := range hit {
			go c.evalSub(sub, false)
		}
	}
}

// ---- rooms ----

func (h *Hub) joinRoom(c *wsConn, room string) {
	h.mu.Lock()
	if h.rooms[c.aid] == nil {
		h.rooms[c.aid] = map[string]map[*wsConn]bool{}
	}
	if h.rooms[c.aid][room] == nil {
		h.rooms[c.aid][room] = map[*wsConn]bool{}
	}
	h.rooms[c.aid][room][c] = true
	h.mu.Unlock()
	c.mu.Lock()
	c.rooms[room] = true
	if c.presence[room] == nil {
		c.presence[room] = map[string]any{}
		c.updated[room] = time.Now().UnixMilli()
	}
	c.mu.Unlock()
	h.broadcastPeers(c.aid, room)
}

func (h *Hub) leaveRoom(c *wsConn, room string) {
	h.mu.Lock()
	if rs := h.rooms[c.aid]; rs != nil {
		delete(rs[room], c)
		if len(rs[room]) == 0 {
			delete(rs, room)
		}
		if len(rs) == 0 {
			delete(h.rooms, c.aid)
		}
	}
	h.mu.Unlock()
	c.mu.Lock()
	delete(c.rooms, room)
	delete(c.presence, room)
	c.mu.Unlock()
	h.broadcastPeers(c.aid, room)
}

func (h *Hub) members(aid, room string) []*wsConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*wsConn
	for c := range h.rooms[aid][room] {
		out = append(out, c)
	}
	return out
}

type peerOut struct {
	Peer      string         `json:"peer"`
	By        *string        `json:"by"`
	IsMe      bool           `json:"isMe"`
	SameTab   bool           `json:"sameTab"`
	Kind      string         `json:"kind"`
	Guest     bool           `json:"guest"`
	Presence  map[string]any `json:"presence,omitempty"`
	UpdatedAt int64          `json:"updatedAt,omitempty"`
}

func senderOf(from, to *wsConn, peer string, caps *Caps) peerOut {
	p := peerOut{Peer: peer, Kind: "viewer"}
	if caps != nil && caps.User != nil && from.user != nil {
		id := from.user.ID
		p.By = &id
	}
	p.SameTab = from == to
	p.IsMe = p.SameTab || (from.user != nil && to.user != nil && from.user.ID == to.user.ID)
	return p
}

func (h *Hub) broadcastPeers(aid, room string) {
	ms := h.members(aid, room)
	type snap struct {
		c        *wsConn
		peer     string
		presence map[string]any
		updated  int64
	}
	snaps := make([]snap, 0, len(ms))
	for _, m := range ms {
		m.mu.Lock()
		snaps = append(snaps, snap{m, m.peer, m.presence[room], m.updated[room]})
		m.mu.Unlock()
	}
	for _, to := range ms {
		to.mu.Lock()
		caps := to.caps
		to.mu.Unlock()
		peers := make([]peerOut, 0, len(snaps))
		for _, sn := range snaps {
			p := senderOf(sn.c, to, sn.peer, caps)
			p.Presence = sn.presence
			if p.Presence == nil {
				p.Presence = map[string]any{}
			}
			p.UpdatedAt = sn.updated
			peers = append(peers, p)
		}
		to.push(map[string]any{"ev": "room.peers", "room": room, "peers": peers})
	}
}

func (h *Hub) broadcastMsg(from *wsConn, room, topic string, data json.RawMessage) {
	from.mu.Lock()
	peer := from.peer
	from.mu.Unlock()
	for _, to := range h.members(from.aid, room) {
		to.mu.Lock()
		caps := to.caps
		to.mu.Unlock()
		msg := map[string]any{"ev": "room.msg", "room": room, "topic": topic, "from": senderOf(from, to, peer, caps)}
		if len(data) > 0 && string(data) != "null" {
			msg["data"] = data
		}
		to.push(msg)
	}
}

// ---- user capability lookups ----

type profileOut struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Email *string `json:"email"`
}

func (h *Hub) profiles(caps *Caps, ids []string) map[string]profileOut {
	out := map[string]profileOut{}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	for _, id := range ids {
		p := profileOut{ID: id}
		if caps.UserScope("profile") || caps.UserScope("email") {
			if u, err := h.s.st.userByID(id); err == nil {
				if caps.UserScope("profile") {
					p.Name = u.DisplayName()
				}
				if caps.UserScope("email") {
					e := u.Email
					p.Email = &e
				}
			}
		}
		out[id] = p
	}
	return out
}

// search finds people who can open the artifact (owner and granted emails).
func (h *Hub) search(c *wsConn, caps *Caps, q string) []profileOut {
	out := []profileOut{}
	if !caps.UserScope("profile") || c.user == nil {
		return out
	}
	a, err := h.s.st.artifact(c.aid)
	if err != nil {
		return out
	}
	cands := map[string]*User{c.user.ID: c.user}
	if u, err := h.s.st.userByID(a.OwnerID); err == nil {
		cands[u.ID] = u
	}
	gs, _ := h.s.st.grants(a.ID)
	for _, g := range gs {
		if strings.HasPrefix(g.Audience, "email:") {
			if u, err := h.s.st.userByEmail(g.Audience[6:]); err == nil {
				cands[u.ID] = u
			}
		}
	}
	ql := strings.ToLower(q)
	for _, u := range cands {
		if q != "" && !strings.Contains(strings.ToLower(u.DisplayName()), ql) && !strings.Contains(strings.ToLower(u.Email), ql) {
			continue
		}
		p := profileOut{ID: u.ID, Name: u.DisplayName()}
		if caps.UserScope("email") {
			e := u.Email
			p.Email = &e
		}
		out = append(out, p)
		if len(out) == 8 {
			break
		}
	}
	return out
}
