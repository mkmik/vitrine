package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Sharing levels. In db/room rules "admin" is the edit level.
const (
	lvNone     = 0
	lvView     = 1
	lvInteract = 2
	lvEdit     = 3 // "admin" in rules
	lvOwner    = 4
	lvSelf     = 5 // a level nobody holds
)

var levelNames = map[string]int{"view": lvView, "interact": lvInteract, "edit": lvEdit, "admin": lvEdit, "owner": lvOwner, "self": lvSelf}

func levelName(l int) string {
	switch l {
	case lvView:
		return "view"
	case lvInteract:
		return "interact"
	case lvEdit:
		return "edit"
	case lvOwner:
		return "owner"
	}
	return "none"
}

type Access struct {
	Level  int
	Pinned int // 0: follows the live head
	User   *User
}

// accessFor computes the viewer's level on an artifact from ownership and grants.
func (s *Store) accessFor(a *Artifact, u *User) Access {
	acc := Access{User: u}
	if u != nil && u.ID == a.OwnerID {
		acc.Level = lvOwner
		return acc
	}
	gs, _ := s.grants(a.ID)
	best, pin := lvNone, 0
	for _, g := range gs {
		match := (g.Audience == "public" && (u != nil || !s.requireSignIn)) || (u != nil && (g.Audience == "users" || g.Audience == "email:"+u.Email))
		if !match {
			continue
		}
		l := levelNames[g.Level]
		p := g.Pinned
		if l >= lvEdit {
			p = 0
		}
		if l > best || (l == best && p == 0) || (l == best && pin != 0 && p > pin) {
			best, pin = l, p
		}
	}
	acc.Level, acc.Pinned = best, pin
	return acc
}

// ---- capability declarations ----

type Rule struct {
	Path  string `json:"path"`
	Read  string `json:"read,omitempty"`
	Write string `json:"write,omitempty"`
}

type DBCap struct {
	Rules []Rule `json:"rules,omitempty"`
}

type RoomCap struct {
	Topics map[string]string `json:"topics,omitempty"`
}

type SampleCap struct {
	Images bool `json:"images,omitempty"`
}

type UserCap struct {
	Scopes []string `json:"scopes,omitempty"`
}

type Caps struct {
	DB     *DBCap
	Room   *RoomCap
	Sample *SampleCap
	User   *UserCap
	Names  []string
}

func (c *Caps) UserScope(s string) bool {
	if c.User == nil {
		return false
	}
	for _, x := range c.User.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

var knownCaps = map[string]bool{"db": true, "room": true, "sample": true, "user": true}

func decodeCap(raw json.RawMessage, v any) error {
	t := strings.TrimSpace(string(raw))
	if t == "true" || t == "{}" || t == "null" || t == "" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func parseCaps(raw json.RawMessage) (*Caps, error) {
	c := &Caps{}
	if len(raw) == 0 {
		return c, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return c, fmt.Errorf("capabilities must be an object: %v", err)
	}
	for k, v := range m {
		if strings.TrimSpace(string(v)) == "false" {
			continue
		}
		var err error
		switch k {
		case "db":
			c.DB = &DBCap{}
			err = decodeCap(v, c.DB)
		case "room":
			c.Room = &RoomCap{}
			err = decodeCap(v, c.Room)
		case "sample":
			c.Sample = &SampleCap{}
			err = decodeCap(v, c.Sample)
		case "user":
			c.User = &UserCap{}
			err = decodeCap(v, c.User)
		default:
			continue // unknown capabilities are kept but inert
		}
		if err != nil {
			return c, fmt.Errorf("%s: %v", k, err)
		}
		c.Names = append(c.Names, k)
	}
	return c, nil
}

var topicRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,47}$`)

// validateCaps checks a declaration and returns warnings for unsupported names.
func validateCaps(raw json.RawMessage) (*Caps, []string, error) {
	c, err := parseCaps(raw)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	var m map[string]json.RawMessage
	json.Unmarshal(raw, &m)
	for k := range m {
		if !knownCaps[k] {
			warnings = append(warnings, fmt.Sprintf("capability %q is not supported by this server and will resolve null", k))
		}
	}
	if c.DB != nil {
		if len(c.DB.Rules) > 64 {
			return nil, nil, fmt.Errorf("db: at most 64 rules")
		}
		selfPrefixes := map[string]bool{}
		for _, r := range c.DB.Rules {
			if strings.HasSuffix(r.Path, "/{self}") {
				selfPrefixes[strings.TrimSuffix(r.Path, "/{self}")] = true
			}
		}
		for _, r := range c.DB.Rules {
			segs := splitPath(r.Path)
			for i, sg := range segs {
				if sg == "{self}" {
					if i != len(segs)-1 {
						return nil, nil, fmt.Errorf("db rule %q: {self} must be the last segment", r.Path)
					}
					continue
				}
				if err := checkSegment(sg); err != nil {
					return nil, nil, fmt.Errorf("db rule %q: %v", r.Path, err)
				}
			}
			for _, lv := range []string{r.Read, r.Write} {
				if lv == "" {
					continue
				}
				if _, ok := levelNames[lv]; !ok || lv == "edit" {
					return nil, nil, fmt.Errorf("db rule %q: unknown level %q (use view, interact, admin, owner, self)", r.Path, lv)
				}
			}
			isSelfRule := strings.HasSuffix(r.Path, "{self}")
			if (r.Read == "self" || r.Write == "self") && (isSelfRule || !selfPrefixes[r.Path]) {
				return nil, nil, fmt.Errorf("db rule %q: level self only goes on the prefix rule of a {self} rule", r.Path)
			}
			if r.Read == "self" && r.Write != "self" {
				return nil, nil, fmt.Errorf("db rule %q: read self requires write self", r.Path)
			}
			if selfPrefixes[r.Path] && (r.Read == "" || r.Write == "") {
				return nil, nil, fmt.Errorf("db rule %q: the prefix rule of a {self} rule must set both read and write", r.Path)
			}
			if r.Read != "" && r.Write != "" && levelNames[r.Write] < levelNames[r.Read] {
				return nil, nil, fmt.Errorf("db rule %q: write level is below read level", r.Path)
			}
		}
	}
	if c.Room != nil {
		if len(c.Room.Topics) > 16 {
			return nil, nil, fmt.Errorf("room: at most 16 topics")
		}
		for t, lv := range c.Room.Topics {
			if !topicRE.MatchString(t) {
				return nil, nil, fmt.Errorf("room topic %q: bad name", t)
			}
			if lv != "interact" && lv != "admin" && lv != "owner" {
				return nil, nil, fmt.Errorf("room topic %q: level must be interact, admin or owner", t)
			}
		}
	}
	if c.User != nil {
		for _, s := range c.User.Scopes {
			if s != "profile" && s != "email" {
				return nil, nil, fmt.Errorf("user: unknown scope %q", s)
			}
		}
	}
	return c, warnings, nil
}

// ---- db access rules ----

type ruleEval struct{ read, write int }

func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// dbLevels returns the minimum levels to read and write a document path for
// the viewer with user id uid (may be "").
func dbLevels(c *Caps, path, uid string) ruleEval {
	segs := splitPath(path)
	ev := ruleEval{read: lvView, write: lvInteract}
	var rules []Rule
	if c != nil && c.DB != nil {
		rules = c.DB.Rules
	}
	// data/users/<id>/ is private unless a rule at data/users opens it.
	selfPrefixes := map[string]bool{"data/users": true}
	declared := map[string]Rule{}
	for _, r := range rules {
		declared[r.Path] = r
		if strings.HasSuffix(r.Path, "/{self}") {
			selfPrefixes[strings.TrimSuffix(r.Path, "/{self}")] = true
		}
	}
	for i := 0; i <= len(segs); i++ {
		prefix := strings.Join(segs[:i], "/")
		if r, ok := declared[prefix]; ok {
			if r.Read != "" {
				ev.read = levelNames[r.Read]
			}
			if r.Write != "" {
				ev.write = levelNames[r.Write]
			}
		}
		if i < len(segs) && selfPrefixes[prefix] {
			owner := segs[i]
			if uid != "" && owner == uid {
				// own subtree: the {self} rule if declared, whichever gives more.
				if sr, ok := declared[prefix+"/{self}"]; ok {
					if sr.Read != "" {
						ev.read = min(ev.read, levelNames[sr.Read])
					}
					if sr.Write != "" {
						ev.write = min(ev.write, levelNames[sr.Write])
					}
				} else {
					if ev.read == lvSelf {
						ev.read = lvView
					}
					if ev.write == lvSelf {
						ev.write = lvInteract
					}
				}
			} else {
				// someone else's subtree: only the prefix rule applies; with none, private.
				if _, ok := declared[prefix]; !ok {
					ev.read, ev.write = lvSelf, lvSelf
				}
				// deeper rules can tighten but never reopen a sibling's subtree
				for j := i + 1; j <= len(segs); j++ {
					if r, ok := declared[strings.Join(segs[:j], "/")]; ok {
						if r.Read != "" && levelNames[r.Read] > ev.read {
							ev.read = levelNames[r.Read]
						}
						if r.Write != "" && levelNames[r.Write] > ev.write {
							ev.write = levelNames[r.Write]
						}
					}
				}
				return ev
			}
		}
	}
	if ev.write < ev.read {
		ev.write = ev.read
	}
	return ev
}

// ---- path grammar ----

var segRE = regexp.MustCompile(`^[A-Za-z0-9_\-.~:@+]+$`)

func checkSegment(s string) error {
	if s == "" {
		return fmt.Errorf("empty segment")
	}
	if s == "." || s == ".." {
		return fmt.Errorf("segment %q not allowed", s)
	}
	if len(s) > 200 {
		return fmt.Errorf("segment over 200 bytes")
	}
	if !segRE.MatchString(s) {
		return fmt.Errorf("segment %q has characters outside letters, digits and _ - . ~ : @ +", s)
	}
	return nil
}

// checkPath validates a slash path; wantDoc selects even (doc) or odd (collection) parity.
func checkPath(p string, wantDoc bool) error {
	if len(p) > 1000 {
		return fmt.Errorf("path over 1000 bytes")
	}
	segs := splitPath(p)
	if len(segs) == 0 {
		return fmt.Errorf("empty path")
	}
	if len(segs) > 16 {
		return fmt.Errorf("path over 16 segments")
	}
	for _, s := range segs {
		if err := checkSegment(s); err != nil {
			return err
		}
	}
	if wantDoc && len(segs)%2 != 0 {
		return fmt.Errorf("document path needs an even number of segments, %q has %d", p, len(segs))
	}
	if !wantDoc && len(segs)%2 != 1 {
		return fmt.Errorf("collection path needs an odd number of segments, %q has %d", p, len(segs))
	}
	return nil
}
