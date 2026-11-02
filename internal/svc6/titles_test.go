package svc6

import (
	"testing"
	"time"

	"github.com/openpak/xb-common/xbfeature"
	"github.com/openpak/xb-voice/internal/xmatch"
)

const (
	titleA = 0x4c41000b
	titleB = 0x4d530004
)

func inTitle(account string, xuid uint64, title uint32) xbfeature.Info {
	i := console(account, xuid)
	i.TitleID = title
	return i
}

func attrInt(tag uint16, v int64) xmatch.Attribute {
	return xmatch.Attribute{Tag: tag, Kind: xmatch.KindInt, Int: v}
}

func host(t *testing.T, s *Server, who xbfeature.Info, ses xmatch.Session) xmatch.Info {
	t.Helper()
	code, body := post(t, s, who, "/xmatch/xmatchhost.srf", ses.Encode())
	if code != 200 {
		t.Fatalf("host: %d", code)
	}
	i, err := xmatch.DecodeInfo(body)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func search(t *testing.T, s *Server, who xbfeature.Info, q xmatch.Search) []xmatch.Result {
	t.Helper()
	code, body := post(t, s, who, "/xmatch/xmatchclient.srf", q.Encode())
	if code != 200 {
		t.Fatalf("search: %d", code)
	}
	r, err := xmatch.DecodeResults(body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTitlePoolsDoNotLeak(t *testing.T) {
	s, fv := newServer()
	alice, bob := inTitle("alice", 0xA, titleA), inTitle("bob", 0xB, titleA)
	bobB := inTitle("bob", 0xB, titleB)
	ia := host(t, s, alice, xmatch.Session{TitleID: titleA, Host: hostAddr(), PublicOpen: 7, PublicFilled: 1, Attributes: []xmatch.Attribute{attrInt(1, 5)}})
	if len(fv.calls) != 0 {
		t.Fatalf("a title session must not touch the voice service: %v", fv.calls)
	}

	// Another title's search and find see nothing; the same title sees it, and a stranger (not a friend) gets the key.
	if r := search(t, s, bobB, xmatch.Search{TitleID: titleB, ProcedureIndex: 1, NumUsers: 1}); len(r) != 0 {
		t.Fatalf("leak across titles: %v", r)
	}
	if r := search(t, s, bobB, xmatch.FindRequest(titleB, xmatch.Addr{}, ia.SessionID)); len(r) != 0 {
		t.Fatalf("find leak across titles: %v", r)
	}
	r := search(t, s, bob, xmatch.Search{TitleID: titleA, ProcedureIndex: 1, NumUsers: 1})
	if len(r) != 1 || r[0].SessionID != ia.SessionID || r[0].Key != ia.Key || r[0].Host != hostAddr() || r[0].PublicOpen != 7 || len(r[0].Attributes) != 1 {
		t.Fatalf("search: %+v", r)
	}
	if r := search(t, s, bob, xmatch.FindRequest(titleA, xmatch.Addr{}, ia.SessionID)); len(r) != 1 {
		t.Fatalf("find by id: %v", r)
	}

	// The same account hosting under title B is a separate session; both coexist.
	aliceB := inTitle("alice", 0xA, titleB)
	ib := host(t, s, aliceB, xmatch.Session{TitleID: titleB, Host: hostAddr(), PublicOpen: 3})
	if ib.SessionID == ia.SessionID || s.Sessions() != 2 {
		t.Fatalf("pools: %d", s.Sessions())
	}
	if r := search(t, s, bob, xmatch.Search{TitleID: titleA, NumUsers: 1}); len(r) != 1 || r[0].SessionID != ia.SessionID {
		t.Fatalf("title A sees %v", r)
	}

	// A delete under the wrong title does nothing; the host's own title deletes.
	if code, _ := post(t, s, aliceB, "/xmatch/xmatchhostdelete.srf", xmatch.Delete{SessionID: ia.SessionID, TitleID: titleB}.Encode()); code != 404 {
		t.Fatalf("cross-title delete: %d", code)
	}
	if code, _ := post(t, s, bob, "/xmatch/xmatchhostdelete.srf", xmatch.Delete{SessionID: ia.SessionID, TitleID: titleA}.Encode()); code != 403 {
		t.Fatalf("non-host delete: %d", code)
	}
	if code, _ := post(t, s, alice, "/xmatch/xmatchhostdelete.srf", xmatch.Delete{SessionID: ia.SessionID, TitleID: titleA}.Encode()); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if r := search(t, s, bob, xmatch.Search{TitleID: titleA, NumUsers: 1}); len(r) != 0 || s.Sessions() != 1 {
		t.Fatalf("after delete: %v", r)
	}
	// A game's title never exposes the dashboard's voice chats.
	host(t, s, console("carol", 0xC), xmatch.Session{TitleID: DashboardTitle, Host: hostAddr(), PrivateOpen: 15})
	if r := search(t, s, bob, xmatch.Search{TitleID: titleA, NumUsers: 0}); len(r) != 0 {
		t.Fatalf("dashboard session visible to a title: %v", r)
	}
}

func TestTitleAttributeMatchingUpdateAndLimits(t *testing.T) {
	s, _ := newServer()
	now := time.Unix(1_700_000_000, 0)
	s.Now = func() time.Time { return now }
	str := xmatch.Attribute{Tag: 2, Kind: xmatch.KindString, Str: "map1"}
	hosts := make([]xmatch.Info, 12)
	for i := range hosts {
		now = now.Add(time.Second)
		who := inTitle(string(rune('a'+i)), uint64(100+i), titleA)
		s.Now = func() time.Time { return now }
		attrs := []xmatch.Attribute{attrInt(1, int64(i%2)), str}
		hosts[i] = host(t, s, who, xmatch.Session{TitleID: titleA, Host: hostAddr(), PublicOpen: uint32(i), Attributes: attrs})
	}
	seeker := inTitle("seeker", 0x5, titleA)
	// Result count limit: 12 sessions, 10 returned, newest first.
	r := search(t, s, seeker, xmatch.Search{TitleID: titleA, ProcedureIndex: 2})
	if len(r) != 10 || r[0].SessionID != hosts[11].SessionID {
		t.Fatalf("limit: %d", len(r))
	}
	// Attribute equality (int tag 1 == 1) and the wildcard.
	r = search(t, s, seeker, xmatch.Search{TitleID: titleA, Attributes: []xmatch.Attribute{attrInt(1, 1), {Tag: 2, Kind: xmatch.KindNull}}})
	if len(r) != 6 {
		t.Fatalf("int match: %d", len(r))
	}
	r = search(t, s, seeker, xmatch.Search{TitleID: titleA, Attributes: []xmatch.Attribute{str, attrInt(1, 0)}})
	if len(r) != 6 {
		t.Fatalf("string+int match: %d", len(r))
	}
	if r = search(t, s, seeker, xmatch.Search{TitleID: titleA, Attributes: []xmatch.Attribute{{Tag: 2, Kind: xmatch.KindString, Str: "map2"}}}); len(r) != 0 {
		t.Fatalf("string mismatch: %d", len(r))
	}
	if r = search(t, s, seeker, xmatch.Search{TitleID: titleA, Attributes: []xmatch.Attribute{attrInt(9, 1)}}); len(r) != 0 {
		t.Fatalf("missing tag: %d", len(r))
	}
	// NumUsers needs that many public open slots (hosts 0..11 have 0..11).
	if r = search(t, s, seeker, xmatch.Search{TitleID: titleA, NumUsers: 11}); len(r) != 1 {
		t.Fatalf("slots: %d", len(r))
	}

	// Host update changes counts and attributes; only the host, and not the address.
	h := inTitle("a", 100, titleA)
	upd := xmatch.Session{SessionID: hosts[0].SessionID, TitleID: titleA, Host: hostAddr(), PublicOpen: 0, PublicFilled: 8, Attributes: []xmatch.Attribute{attrInt(1, 7)}}
	if code, _ := post(t, s, h, "/xmatch/xmatchhost.srf", upd.Encode()); code != 200 {
		t.Fatalf("update: %d", code)
	}
	r = search(t, s, seeker, xmatch.Search{TitleID: titleA, Attributes: []xmatch.Attribute{attrInt(1, 7)}})
	if len(r) != 1 || r[0].PublicFilled != 8 || r[0].SessionID != hosts[0].SessionID {
		t.Fatalf("after update: %+v", r)
	}
	if code, _ := post(t, s, seeker, "/xmatch/xmatchhost.srf", upd.Encode()); code != 403 {
		t.Fatalf("non-host update: %d", code)
	}
	moved := upd
	moved.Host[0] = 99
	if code, _ := post(t, s, h, "/xmatch/xmatchhost.srf", moved.Encode()); code != 409 {
		t.Fatalf("host address change: %d", code)
	}
	if code, _ := post(t, s, inTitle("a", 100, titleB), "/xmatch/xmatchhost.srf", upd.Encode()); code != 501 {
		t.Fatalf("update under another connection title: %d", code)
	}
}

func TestTitleSessionExpiryNeedsHostRefresh(t *testing.T) {
	s, _ := newServer()
	now := time.Unix(1_700_000_000, 0)
	s.Now = func() time.Time { return now }
	alice, bob := inTitle("alice", 0xA, titleA), inTitle("bob", 0xB, titleA)
	ia := host(t, s, alice, xmatch.Session{TitleID: titleA, Host: hostAddr(), PublicOpen: 4})
	q := xmatch.Search{TitleID: titleA, NumUsers: 1}

	// Searches by others do not keep it alive; a host update does.
	now = now.Add(10 * time.Minute)
	if len(search(t, s, bob, q)) != 1 {
		t.Fatal("still fresh at 10 min")
	}
	upd := xmatch.Session{SessionID: ia.SessionID, TitleID: titleA, Host: hostAddr(), PublicOpen: 4}
	if code, _ := post(t, s, alice, "/xmatch/xmatchhost.srf", upd.Encode()); code != 200 {
		t.Fatalf("refresh: %d", code)
	}
	now = now.Add(10 * time.Minute)
	if len(search(t, s, bob, q)) != 1 {
		t.Fatal("refresh must extend the life")
	}
	now = now.Add(16 * time.Minute)
	if r := search(t, s, bob, q); len(r) != 0 || s.Sessions() != 0 {
		t.Fatalf("unrefreshed session must expire: %v %d", r, s.Sessions())
	}
	if code, _ := post(t, s, alice, "/xmatch/xmatchhost.srf", upd.Encode()); code != 404 {
		t.Fatalf("update after expiry: %d", code)
	}
}

func TestTitleRecreateReplacesAndDashboardUntouched(t *testing.T) {
	s, fv := newServer()
	alice := inTitle("alice", 0xA, titleA)
	i1 := host(t, s, alice, xmatch.Session{TitleID: titleA, Host: hostAddr(), PublicOpen: 2})
	i2 := host(t, s, alice, xmatch.Session{TitleID: titleA, Host: hostAddr(), PublicOpen: 2})
	if i1.SessionID == i2.SessionID || s.Sessions() != 1 {
		t.Fatalf("recreate: %d", s.Sessions())
	}
	// The dashboard's chat for the same account is a separate record and still mirrors into voice.
	host(t, s, console("alice", 0xA), xmatch.Session{TitleID: DashboardTitle, Host: hostAddr(), PrivateOpen: 15, PublicFilled: 1, PrivateFilled: 1})
	if s.Sessions() != 2 || len(fv.calls) != 1 {
		t.Fatalf("dashboard: sessions=%d calls=%v", s.Sessions(), fv.calls)
	}
}
