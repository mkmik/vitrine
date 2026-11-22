package main

import (
	"encoding/json"
	"net/http"
)

// apiDB runs one db call over plain HTTP: the shell's fallback when its
// WebSocket cannot connect (listeners then refresh by polling).
func (s *Server) apiDB(w http.ResponseWriter, r *http.Request) {
	ss, a, acc := s.loadForAPI(w, r, lvView)
	if a == nil {
		return
	}
	reply := func(code int, res any, e *DBError) {
		if e != nil {
			writeJSON(w, code, map[string]any{"error": e})
			return
		}
		writeJSON(w, 200, map[string]any{"result": res})
	}
	caps := a.Caps()
	if caps.DB == nil {
		reply(403, nil, dbErr("not_granted", "db is not available in this view"))
		return
	}
	if !s.allow("dbhttp:"+ss.User.ID, 20, 100) {
		reply(429, nil, dbErr("resource_exhausted", "too many calls"))
		return
	}
	var m wsMsg
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&m); err != nil {
		reply(400, nil, dbErr("invalid_argument", "bad json"))
		return
	}
	v := Viewer{UID: ss.User.ID, Level: acc.Level}
	switch m.Op {
	case "db.get":
		d, e := s.st.docGet(a.ID, caps, v, m.Path)
		reply(400, d, e)
	case "db.query":
		if m.Q == nil {
			reply(400, nil, dbErr("invalid_argument", "missing query"))
			return
		}
		docs, e := s.st.docQuery(a.ID, caps, v, *m.Q)
		if e != nil {
			reply(400, nil, e)
			return
		}
		reply(200, map[string]any{"docs": docs}, nil)
	case "db.write":
		if m.W == nil {
			reply(400, nil, dbErr("invalid_argument", "missing write"))
			return
		}
		res, e := s.st.docWrite(a.ID, caps, v, *m.W, s.cfg.MaxDocBytes, s.cfg.MaxDocs)
		if e == nil {
			s.hub.docChanged(a.ID, m.W.Path)
		}
		reply(400, res, e)
	case "db.acquire":
		var data map[string]any
		if len(m.Data) > 0 && string(m.Data) != "null" {
			if json.Unmarshal(m.Data, &data) != nil {
				reply(400, nil, dbErr("invalid_argument", "data must be an object"))
				return
			}
		}
		res, e := s.st.docAcquire(a.ID, caps, v, m.Path, m.Holder, m.TTL, data, s.cfg.MaxDocBytes)
		if e == nil && res.Acquired {
			s.hub.docChanged(a.ID, m.Path)
		}
		reply(400, res, e)
	default:
		reply(400, nil, dbErr("invalid_argument", "unknown op %q", m.Op))
	}
}
