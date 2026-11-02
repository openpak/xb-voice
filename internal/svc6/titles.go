// SPDX-License-Identifier: AGPL-3.0-only

package svc6

import (
	"bytes"
	"crypto/rand"
	"errors"
	"log"
	"slices"

	"github.com/openpak/xb-common/xbfeature"
	"github.com/openpak/xb-voice/internal/xmatch"
)

// Title sessions: every title other than the dashboard has its own pool. The
// record's title is the only key into it, so a session hosted under one title
// is invisible to every other title's searches, lookups, updates and deletes.
//
// What is from Xombie (libs/xombie-matchmaking): sessions are per title, a
// search returns the title's sessions, at most 10. What is inferred here
// (Xombie leaves attribute and user-count matching as a TODO, and the real
// service ran title-defined server procedures we have no capture of): the
// procedure index is ignored, a search attribute matches a session attribute
// with the same tag, type and value, a null-typed ("any") search attribute
// matches everything, and the session needs at least NumUsers public open slots.

const (
	// maxResults is Xombie's search limit.
	maxResults = 10
	// maxTitleSessions bounds memory: one process-wide cap on sessions.
	maxTitleSessions = 20000
)

type titleHost struct {
	host  string
	title uint32
}

func (s *Server) titleCreate(info xbfeature.Info, ses xmatch.Session) (int, []byte, error) {
	var key xmatch.Key
	if _, err := rand.Read(key[:]); err != nil {
		return 500, nil, err
	}
	id := newID()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	// A host re-creating its session for a title replaces the old one.
	if old, ok := s.byTitleHost[titleHost{info.CoreAccountID, ses.TitleID}]; ok {
		delete(s.byID, old)
	}
	if len(s.byID) >= maxTitleSessions {
		return 503, nil, errors.New("too many sessions")
	}
	r := &record{id: id, key: key, host: info.CoreAccountID, hostXUID: info.XUID, title: ses.TitleID, addr: ses.Host,
		pub: [2]uint32{ses.PublicOpen, ses.PublicFilled}, priv: [2]uint32{ses.PrivateOpen, ses.PrivateFilled},
		attrs: ses.Attributes, created: now, touched: now}
	s.byID[id] = r
	s.byTitleHost[titleHost{r.host, r.title}] = id
	log.Printf("svc6: title session created title=%08x gen=%d open=%d/%d filled=%d/%d attrs=%d", ses.TitleID, info.Generation, ses.PublicOpen, ses.PrivateOpen, ses.PublicFilled, ses.PrivateFilled, len(ses.Attributes))
	return 200, xmatch.Info{SessionID: id, Key: key}.Encode(), nil
}

func (s *Server) titleSearch(q xmatch.Search) (int, []byte, error) {
	var out []*record
	s.mu.Lock()
	s.init()
	if id, ok := q.FindID(); ok {
		if r := s.byID[id]; r != nil && r.title == q.TitleID {
			out = append(out, r)
		}
	} else {
		for _, r := range s.byID {
			if r.title == q.TitleID && matches(r, q) {
				out = append(out, r)
			}
		}
		// Newest first, id as the tie break, so the answer is stable.
		slices.SortFunc(out, func(a, b *record) int {
			if c := b.created.Compare(a.created); c != 0 {
				return c
			}
			return bytes.Compare(a.id[:], b.id[:])
		})
		out = out[:min(len(out), maxResults)]
	}
	res := make([]xmatch.Result, len(out))
	for i, r := range out {
		res[i] = xmatch.Result{SessionID: r.id, Host: r.addr, Key: r.key,
			PublicOpen: r.pub[0], PrivateOpen: r.priv[0], PublicFilled: r.pub[1], PrivateFilled: r.priv[1], Attributes: r.attrs}
	}
	s.mu.Unlock()
	return 200, xmatch.EncodeResults(0, res), nil
}

func matches(r *record, q xmatch.Search) bool {
	if int64(r.pub[0]) < int64(q.NumUsers) {
		return false
	}
	for _, want := range q.Attributes {
		if want.Kind == xmatch.KindNull {
			continue
		}
		if !slices.ContainsFunc(r.attrs, func(have xmatch.Attribute) bool {
			return have.Tag == want.Tag && have.Kind == want.Kind && have.Int == want.Int && have.Str == want.Str && bytes.Equal(have.Blob, want.Blob)
		}) {
			return false
		}
	}
	return true
}
