package svc6

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
	"github.com/openpak/xb-common/xbfeature"
	"github.com/openpak/xb-voice/internal/xmatch"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeVoice models the voice service's rules that matter here: friends of the
// owner may join, anyone else is refused, ended sessions are gone.
type fakeVoice struct {
	mu      sync.Mutex
	friends map[string]bool // "joiner>owner"
	nextID  int
	ses     map[string]*fakeSession
	calls   []string
	down    bool
}

type fakeSession struct {
	owner      string
	ref        string
	members    map[string]bool
	ended      bool
	endOnOwner bool
}

func newVoice() *fakeVoice {
	return &fakeVoice{friends: map[string]bool{}, ses: map[string]*fakeSession{}}
}

func (f *fakeVoice) log(s string, a ...any) { f.calls = append(f.calls, fmt.Sprintf(s, a...)) }

func (f *fakeVoice) CreateSession(_ context.Context, in *voicev1.CreateSessionRequest, _ ...grpc.CallOption) (*voicev1.CreateSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, status.Error(codes.Unavailable, "down")
	}
	f.nextID++
	id := fmt.Sprintf("s%d", f.nextID)
	f.ses[id] = &fakeSession{owner: in.GetOwnerAccount(), ref: in.GetNativeRef(), members: map[string]bool{in.GetOwnerAccount(): true}, endOnOwner: in.GetEndWhenOwnerLeaves()}
	f.log("create %s owner=%s ref=%s kind=%v max=%d transport=%v", id, in.GetOwnerAccount(), in.GetNativeRef(), in.GetKind(), in.GetMaxParticipants(), in.GetTransport())
	return &voicev1.CreateSessionResponse{Session: &voicev1.Session{Id: id}}, nil
}

func (f *fakeVoice) JoinSession(_ context.Context, in *voicev1.JoinSessionRequest, _ ...grpc.CallOption) (*voicev1.JoinSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, status.Error(codes.Unavailable, "down")
	}
	s := f.ses[in.GetSessionId()]
	if s == nil {
		return nil, status.Error(codes.NotFound, "no such session")
	}
	if s.ended {
		return nil, status.Error(codes.FailedPrecondition, "ended")
	}
	f.log("join %s %s", in.GetSessionId(), in.GetAccountId())
	if !f.friends[in.GetAccountId()+">"+s.owner] {
		return nil, status.Error(codes.PermissionDenied, "NOT_FRIENDS")
	}
	s.members[in.GetAccountId()] = true
	return &voicev1.JoinSessionResponse{}, nil
}

func (f *fakeVoice) Heartbeat(_ context.Context, in *voicev1.HeartbeatRequest, _ ...grpc.CallOption) (*voicev1.HeartbeatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("heartbeat %s %s", in.GetSessionId(), in.GetAccountId())
	return &voicev1.HeartbeatResponse{}, nil
}

func (f *fakeVoice) LeaveSession(context.Context, *voicev1.LeaveSessionRequest, ...grpc.CallOption) (*voicev1.LeaveSessionResponse, error) {
	return &voicev1.LeaveSessionResponse{}, nil
}

func (f *fakeVoice) EndSession(_ context.Context, in *voicev1.EndSessionRequest, _ ...grpc.CallOption) (*voicev1.EndSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.ses[in.GetSessionId()]
	if s == nil || s.owner != in.GetActorAccount() {
		return nil, status.Error(codes.PermissionDenied, "not the owner")
	}
	s.ended = true
	f.log("end %s", in.GetSessionId())
	return &voicev1.EndSessionResponse{}, nil
}

type pipe struct {
	in  chan []byte
	out bytes.Buffer
}

func (p *pipe) Recv() ([]byte, error) {
	b, ok := <-p.in
	if !ok {
		return nil, io.EOF
	}
	return b, nil
}
func (p *pipe) Send(b []byte) error { p.out.Write(b); return nil }

// post sends one console request on a fresh connection and returns the status
// and body of the reply.
func post(t *testing.T, s *Server, info xbfeature.Info, path string, body []byte, hdr ...string) (int, []byte) {
	t.Helper()
	ct := xmatch.ContentType
	method := "POST"
	for _, h := range hdr {
		switch h[:2] {
		case "ct":
			ct = h[3:]
		case "me":
			method = h[3:]
		}
	}
	p := &pipe{in: make(chan []byte, 1)}
	p.in <- []byte(fmt.Sprintf("%s %s HTTP/1.0\r\nUser-Agent: 1/1.0\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", method, path, ct, len(body)))
	// Deliver in two chunks like the SG does.
	go func() { p.in <- body; close(p.in) }()
	if len(body) == 0 {
		// no second chunk needed, but the goroutine still sends an empty one
	}
	if err := s.Serve(context.Background(), info, p); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(&p.out), nil)
	if err != nil {
		t.Fatalf("response: %v (%q)", err, p.out.String())
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func console(account string, xuid uint64) xbfeature.Info {
	return xbfeature.Info{CoreAccountID: account, XUID: xuid, ServiceID: 6, Generation: 3, PrincipalID: "p"}
}

func hostAddr() xmatch.Addr {
	var a xmatch.Addr
	copy(a[:], []byte{10, 0, 2, 15, 203, 0, 113, 9, 0x90, 0x73, 0, 0x50, 0xf2, 1, 2, 3, 0xAA, 0xBB, 0xCC})
	return a
}

func createBody() []byte {
	return xmatch.Session{TitleID: DashboardTitle, Host: hostAddr(), PrivateOpen: 15, PublicFilled: 1, PrivateFilled: 1}.Encode()
}

func newServer() (*Server, *fakeVoice) {
	fv := newVoice()
	fv.friends["bob>alice"] = true
	return &Server{Voice: fv, Now: time.Now}, fv
}

func TestHostFriendStranger(t *testing.T) {
	s, fv := newServer()
	alice, bob, eve := console("alice", 0xA), console("bob", 0xB), console("eve", 0xE)

	code, body := post(t, s, alice, "/xmatch/xmatchhost.srf", createBody())
	if code != 200 || len(body) != 24 {
		t.Fatalf("create: %d %x", code, body)
	}
	info, _ := xmatch.DecodeInfo(body)
	if info.SessionID.IsZero() || info.Key == (xmatch.Key{}) {
		t.Fatal("the server must hand out a real id and key")
	}
	if got := fv.calls[0]; got != fmt.Sprintf("create s1 owner=alice ref=%s kind=SESSION_KIND_NATIVE max=16 transport=TRANSPORT_NATIVE_PEER", hex.EncodeToString(info.SessionID[:])) {
		t.Fatalf("voice create: %s", got)
	}

	find := xmatch.FindRequest(DashboardTitle, xmatch.Addr{}, info.SessionID).Encode()
	// A friend gets the host's address and the key.
	code, body = post(t, s, bob, "/xmatch/xmatchclient.srf", find)
	if code != 200 {
		t.Fatalf("find: %d", code)
	}
	res, err := xmatch.DecodeResults(body)
	if err != nil || len(res) != 1 {
		t.Fatalf("results: %v %v", res, err)
	}
	if res[0].SessionID != info.SessionID || res[0].Key != info.Key || res[0].Host != hostAddr() || res[0].PrivateOpen != 15 || res[0].PublicFilled != 1 {
		t.Fatalf("result %+v", res[0])
	}
	// A stranger gets exactly what a missing session gets, and no key.
	code, body = post(t, s, eve, "/xmatch/xmatchclient.srf", find)
	res, _ = xmatch.DecodeResults(body)
	if code != 200 || len(res) != 0 || bytes.Contains(body, info.Key[:]) {
		t.Fatalf("stranger: %d %v", code, res)
	}
	code, body = post(t, s, bob, "/xmatch/xmatchclient.srf", xmatch.FindRequest(DashboardTitle, xmatch.Addr{}, xmatch.ID{1, 2, 3}).Encode())
	if res, _ = xmatch.DecodeResults(body); code != 200 || len(res) != 0 {
		t.Fatalf("unknown id: %d %v", code, res)
	}
	// The host looking up its own chat is not a second participant.
	before := len(fv.calls)
	post(t, s, alice, "/xmatch/xmatchclient.srf", find)
	for _, c := range fv.calls[before:] {
		if c == "join s1 alice" {
			t.Fatal("the host must not join its own session")
		}
	}
}

func TestOnlyTheDashboardAndOnlyFindByID(t *testing.T) {
	s, _ := newServer()
	alice := console("alice", 0xA)
	game := xmatch.Session{TitleID: 0x4c41000b, Host: hostAddr()}.Encode()
	if code, _ := post(t, s, alice, "/xmatch/xmatchhost.srf", game); code != 501 {
		t.Fatalf("a game's session: %d", code)
	}
	search := xmatch.Search{TitleID: DashboardTitle, ProcedureIndex: 1}.Encode()
	if code, _ := post(t, s, alice, "/xmatch/xmatchclient.srf", search); code != 501 {
		t.Fatalf("a search other than find-by-id: %d", code)
	}
	gameSearch := xmatch.FindRequest(0x4c41000b, xmatch.Addr{}, xmatch.ID{1}).Encode()
	if code, _ := post(t, s, alice, "/xmatch/xmatchclient.srf", gameSearch); code != 501 {
		t.Fatalf("a game's lookup: %d", code)
	}
	mismatch := console("alice", 0xA)
	mismatch.TitleID = 0x4c41000b
	if code, _ := post(t, s, mismatch, "/xmatch/xmatchhost.srf", createBody()); code != 501 {
		t.Fatalf("the connection's title must agree: %d", code)
	}
}

func TestRequestChecks(t *testing.T) {
	s, _ := newServer()
	alice := console("alice", 0xA)
	machine := console("", 0x1)
	cases := []struct {
		name string
		info xbfeature.Info
		path string
		body []byte
		hdr  []string
		want int
	}{
		{"machine connection", machine, "/xmatch/xmatchhost.srf", createBody(), nil, 403},
		{"no user", xbfeature.Info{ServiceID: 6, CoreAccountID: "alice"}, "/xmatch/xmatchhost.srf", createBody(), nil, 403},
		{"wrong content type", alice, "/xmatch/xmatchhost.srf", createBody(), []string{"ct=xon/1"}, 400},
		{"wrong method", alice, "/xmatch/xmatchhost.srf", nil, []string{"me=GET"}, 405},
		{"unknown path", alice, "/xmatch/other.srf", createBody(), nil, 404},
		{"garbage", alice, "/xmatch/xmatchhost.srf", []byte("nope"), nil, 400},
		{"garbage delete", alice, "/xmatch/xmatchhostdelete.srf", []byte{1, 2, 3}, nil, 400},
		{"wrong service", xbfeature.Info{ServiceID: 5, CoreAccountID: "alice", XUID: 1}, "/xmatch/xmatchhost.srf", createBody(), nil, -1},
	}
	for _, c := range cases {
		if c.want == -1 {
			if err := s.Serve(context.Background(), c.info, &pipe{in: make(chan []byte)}); !errors.Is(err, errNoBackend) {
				t.Fatalf("%s: %v", c.name, err)
			}
			continue
		}
		if code, _ := post(t, s, c.info, c.path, c.body, c.hdr...); code != c.want {
			t.Fatalf("%s: got %d want %d", c.name, code, c.want)
		}
	}
}

func TestUpdateDeleteReplace(t *testing.T) {
	s, fv := newServer()
	alice, bob := console("alice", 0xA), console("bob", 0xB)
	_, body := post(t, s, alice, "/xmatch/xmatchhost.srf", createBody())
	info, _ := xmatch.DecodeInfo(body)

	upd := xmatch.Session{SessionID: info.SessionID, TitleID: DashboardTitle, Host: hostAddr(), PrivateOpen: 14, PublicFilled: 1, PrivateFilled: 2}
	code, body := post(t, s, alice, "/xmatch/xmatchhost.srf", upd.Encode())
	if got, _ := xmatch.DecodeInfo(body); code != 200 || got != info {
		t.Fatalf("update must return the same id and key: %d %v", code, got)
	}
	if code, _ := post(t, s, bob, "/xmatch/xmatchhost.srf", upd.Encode()); code != 403 {
		t.Fatalf("update by another console: %d", code)
	}
	moved := upd
	moved.Host[4] = 99
	if code, _ := post(t, s, alice, "/xmatch/xmatchhost.srf", moved.Encode()); code != 409 {
		t.Fatalf("host address change: %d", code)
	}
	upd.SessionID = xmatch.ID{7}
	if code, _ := post(t, s, alice, "/xmatch/xmatchhost.srf", upd.Encode()); code != 404 {
		t.Fatalf("unknown session update: %d", code)
	}

	// The counts the host reports are what a joiner sees.
	_, body = post(t, s, bob, "/xmatch/xmatchclient.srf", xmatch.FindRequest(DashboardTitle, xmatch.Addr{}, info.SessionID).Encode())
	if res, _ := xmatch.DecodeResults(body); len(res) != 1 || res[0].PrivateOpen != 14 || res[0].PrivateFilled != 2 {
		t.Fatalf("%+v", res)
	}

	// A second create by the host replaces the first.
	_, body2 := post(t, s, alice, "/xmatch/xmatchhost.srf", createBody())
	info2, _ := xmatch.DecodeInfo(body2)
	if info2.SessionID == info.SessionID || s.Sessions() != 1 || !fv.ses["s1"].ended {
		t.Fatalf("replace: %v sessions=%d ended=%v", info2, s.Sessions(), fv.ses["s1"].ended)
	}

	del := xmatch.Delete{SessionID: info2.SessionID, TitleID: DashboardTitle}.Encode()
	if code, _ := post(t, s, bob, "/xmatch/xmatchhostdelete.srf", del); code != 403 {
		t.Fatalf("delete by another console: %d", code)
	}
	if code, _ := post(t, s, alice, "/xmatch/xmatchhostdelete.srf", del); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := post(t, s, alice, "/xmatch/xmatchhostdelete.srf", del); code != 404 {
		t.Fatalf("second delete: %d", code)
	}
	_, body = post(t, s, bob, "/xmatch/xmatchclient.srf", xmatch.FindRequest(DashboardTitle, xmatch.Addr{}, info2.SessionID).Encode())
	if res, _ := xmatch.DecodeResults(body); len(res) != 0 || !fv.ses["s2"].ended {
		t.Fatalf("a deleted session must not resolve: %v", res)
	}
}

func TestIdleExpiryAndVoiceDown(t *testing.T) {
	s, fv := newServer()
	now := time.Unix(1_700_000_000, 0)
	s.Now = func() time.Time { return now }
	alice, bob := console("alice", 0xA), console("bob", 0xB)
	_, body := post(t, s, alice, "/xmatch/xmatchhost.srf", createBody())
	info, _ := xmatch.DecodeInfo(body)
	find := xmatch.FindRequest(DashboardTitle, xmatch.Addr{}, info.SessionID).Encode()

	// The voice service being down is 503, not "no such session", and not a leak.
	fv.down = true
	if code, b := post(t, s, bob, "/xmatch/xmatchclient.srf", find); code != 503 || len(b) != 0 {
		t.Fatalf("voice down: %d %x", code, b)
	}
	if code, _ := post(t, s, console("carol", 0xC), "/xmatch/xmatchhost.srf", createBody()); code != 503 {
		t.Fatalf("create with voice down: %d", code)
	}
	fv.down = false

	now = now.Add(3 * time.Hour)
	_, body = post(t, s, bob, "/xmatch/xmatchclient.srf", find)
	if res, _ := xmatch.DecodeResults(body); len(res) != 0 || s.Sessions() != 0 || !fv.ses["s1"].ended {
		t.Fatalf("an idle session expires: %v sessions=%d", res, s.Sessions())
	}
}

func TestRateLimit(t *testing.T) {
	s, _ := newServer()
	alice := console("alice", 0xA)
	find := xmatch.FindRequest(DashboardTitle, xmatch.Addr{}, xmatch.ID{1}).Encode()
	var limited bool
	for i := 0; i < 40; i++ {
		if code, _ := post(t, s, alice, "/xmatch/xmatchclient.srf", find); code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Fatal("a console hammering lookups must be limited")
	}
}

func TestRedactionKeepsConsoleIdentifiersOutOfLogs(t *testing.T) {
	body := createBody()
	red := redactBody("/xmatch/xmatchhost.srf", body)
	if bytes.Equal(red, body) {
		t.Fatal("redaction changed nothing")
	}
	if !bytes.Equal(red[:16+10], body[:16+10]) || !bytes.Equal(red[16+xmatch.AddrLen:], body[16+xmatch.AddrLen:]) {
		t.Fatal("only the MAC and machine block are zeroed")
	}
	for _, b := range red[16+10 : 16+xmatch.AddrLen] {
		if b != 0 {
			t.Fatal("identifying bytes survived")
		}
	}
}
