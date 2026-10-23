// SPDX-License-Identifier: AGPL-3.0-only

// Package svc6 serves native service 6 (xmatch) to the gateway for the
// original Xbox dashboard's VOICE CHAT sessions, over xbfeature streams
// (POST /xmatch/xmatch{host,client,hostdelete}.srf, Content-Type xon/6).
//
// What the dashboard does (internal research notes): the host registers a
// session and gets a session id and a key back; friends are invited through the
// presence service; a friend looks the session up by id and receives the host's
// network address and the key, then connects to the host console directly. The
// audio never touches a server. This package is therefore a session registry
// with real authorization, not a media path:
//
//   - the session is mirrored into the neutral voice service (internal/session
//     there), which asks the account core whether a looker-up may join;
//   - a refused looker-up gets the same answer as a session that does not exist
//     (an empty result), which the console shows as "no longer available";
//   - the key and the host's address are only ever handed to an admitted friend.
//
// Only the dashboard's own title (0xFFFE0000) is served. Every other title's
// matchmaking is answered 501: another service has to own that, and it is not
// pretended here.
package svc6

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
	"github.com/openpak/xb-common/xbfeature"
	"github.com/openpak/xb-voice/internal/xmatch"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DashboardTitle is the title id of xonlinedash.xbe (its certificate).
const DashboardTitle = 0xFFFE0000

const (
	// NativeNamespace is this adapter's namespace in the voice service.
	NativeNamespace = "xbox"
	// serviceID of xmatch.
	serviceID = 6

	pathHost   = "/xmatch/xmatchhost.srf"
	pathClient = "/xmatch/xmatchclient.srf"
	pathDelete = "/xmatch/xmatchhostdelete.srf"

	maxBody = xmatch.MaxBody
)

// Voice is the part of the voice service's client this package uses.
type Voice interface {
	CreateSession(ctx context.Context, in *voicev1.CreateSessionRequest, opts ...grpc.CallOption) (*voicev1.CreateSessionResponse, error)
	JoinSession(ctx context.Context, in *voicev1.JoinSessionRequest, opts ...grpc.CallOption) (*voicev1.JoinSessionResponse, error)
	Heartbeat(ctx context.Context, in *voicev1.HeartbeatRequest, opts ...grpc.CallOption) (*voicev1.HeartbeatResponse, error)
	LeaveSession(ctx context.Context, in *voicev1.LeaveSessionRequest, opts ...grpc.CallOption) (*voicev1.LeaveSessionResponse, error)
	EndSession(ctx context.Context, in *voicev1.EndSessionRequest, opts ...grpc.CallOption) (*voicev1.EndSessionResponse, error)
}

// record is one hosted session: the native secrets and address live here, never
// in the voice service.
type record struct {
	id        xmatch.ID
	key       xmatch.Key
	coreID    string
	host      string // core account id
	hostXUID  uint64
	title     uint32
	addr      xmatch.Addr
	pub, priv [2]uint32 // open, filled
	attrs     []xmatch.Attribute
	created   time.Time
	touched   time.Time
	members   map[string]time.Time // core account -> last lookup
}

// Server implements xbfeature.Handler for service 6.
type Server struct {
	Voice Voice
	// TTL is how long a session stays resolvable without any message from its
	// host or a joiner (default 2h). Native membership is console to console, so
	// the server only learns of activity, never of a console leaving.
	TTL time.Duration
	// JoinLease is how long a looked-up member stays a participant in the voice
	// service (default 10 min).
	JoinLease time.Duration
	// DebugRequests logs each request body as hex with the console-identifying
	// bytes of every address zeroed. The session key is never logged.
	DebugRequests bool
	Now           func() time.Time

	mu      sync.Mutex
	byID    map[xmatch.ID]*record
	byHost  map[string]xmatch.ID
	buckets map[uint64]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return 2 * time.Hour
}

func (s *Server) joinLease() time.Duration {
	if s.JoinLease > 0 {
		return s.JoinLease
	}
	return 10 * time.Minute
}

func (s *Server) init() {
	if s.byID == nil {
		s.byID = map[xmatch.ID]*record{}
		s.byHost = map[string]xmatch.ID{}
		s.buckets = map[uint64]*bucket{}
	}
}

func (s *Server) allow(xuid uint64) bool {
	const burst, refill = 20, 3.0
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	b := s.buckets[xuid]
	if b == nil {
		if len(s.buckets) > 4096 {
			for k, v := range s.buckets {
				if now.Sub(v.last) > time.Minute {
					delete(s.buckets, k)
				}
			}
		}
		b = &bucket{tokens: burst, last: now}
		s.buckets[xuid] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()/refill)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type connReader struct {
	c   xbfeature.Conn
	buf []byte
}

func (r *connReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		b, err := r.c.Recv()
		if err != nil {
			return 0, err
		}
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

var errNoBackend = errors.New("svc6: service id not served here")

// Serve handles one console connection (xbfeature.Handler).
func (s *Server) Serve(ctx context.Context, info xbfeature.Info, c xbfeature.Conn) error {
	if info.ServiceID != serviceID {
		return errNoBackend
	}
	br := bufio.NewReaderSize(&connReader{c: c}, 8<<10)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return respond(c, 400, nil)
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
		req.Body.Close()
		if err != nil || len(body) > maxBody {
			return respond(c, 413, nil)
		}
		code, out := s.handle(ctx, info, req, body)
		closeConn := req.Close || req.ProtoMajor == 1 && req.ProtoMinor == 0 || code >= 400
		if err := respond(c, code, out); err != nil || closeConn {
			return err
		}
	}
}

func respond(c xbfeature.Conn, code int, body []byte) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/1.0 %d %s\r\n", code, http.StatusText(code))
	if code == 200 {
		b.WriteString("X-Err: 0\r\nContent-Type: " + xmatch.ContentType + "\r\n")
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.Write(body)
	out := b.Bytes()
	for len(out) > 0 {
		n := min(len(out), 16<<10)
		if err := c.Send(out[:n]); err != nil {
			return err
		}
		out = out[n:]
	}
	return nil
}

func (s *Server) handle(ctx context.Context, info xbfeature.Info, req *http.Request, body []byte) (int, []byte) {
	path := strings.ToLower(req.URL.Path)
	if path != pathHost && path != pathClient && path != pathDelete {
		return 404, nil
	}
	if req.Header.Get("Content-Type") != xmatch.ContentType {
		return 400, nil
	}
	if req.Method != http.MethodPost {
		return 405, nil
	}
	// A session belongs to a signed-in user: the identity is what the gateway
	// bound from the ticket, never anything in the body.
	if info.XUID == 0 || info.CoreAccountID == "" {
		return 403, nil
	}
	if !s.allow(info.XUID) {
		return 429, nil
	}
	s.expire(ctx)
	var (
		code int
		out  []byte
		err  error
	)
	switch path {
	case pathHost:
		code, out, err = s.host(ctx, info, body)
	case pathClient:
		code, out, err = s.client(ctx, info, body)
	default:
		code, out, err = s.remove(ctx, info, body)
	}
	if err != nil {
		log.Printf("svc6: %s: %v", path, err)
	}
	return code, out
}

func (s *Server) debug(info xbfeature.Info, path string, body []byte) {
	if s.DebugRequests {
		log.Printf("svc6: request path=%s gen=%d len=%d body=%x", path, info.Generation, len(body), redactBody(path, body))
	}
}

// redactBody zeroes the console-identifying part of every XNADDR in a request.
func redactBody(path string, b []byte) []byte {
	out := append([]byte(nil), b...)
	zero := func(off int) {
		if off+xmatch.AddrLen <= len(out) {
			for i := off + 10; i < off+xmatch.AddrLen; i++ {
				out[i] = 0
			}
		}
	}
	switch path {
	case pathHost:
		zero(16)
	case pathClient:
		zero(12)
	}
	return out
}

// newID makes a session id the dashboard accepts: XNetRegisterKey takes only the key
// types 0x00, 0x80 and 0xE0 in the top three bits of the first byte and answers 0x2726
// for any other, and 0x80 is the online peer type. Bytes 1..7 are never all zero, so a
// generated id can never match isCreateFlags (which needs id[1:] zero), whatever the
// random bytes were.
func newID() (id xmatch.ID) {
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return keyType(id)
}

func keyType(id xmatch.ID) xmatch.ID {
	id[0] = 0x80 | id[0]&0x1f
	if id == (xmatch.ID{id[0]}) {
		id[7] = 1
	}
	return id
}

// isCreateFlags reports whether id has the shape the dashboard puts in the id slot of a
// create request: flags<<4 in the first byte, the rest zero (captured 2026-10-18: 10 00 ..).
func isCreateFlags(id xmatch.ID) bool {
	low := id[0] & 0x0f
	id[0] = 0
	return low == 0 && id == xmatch.ID{}
}

func (s *Server) host(ctx context.Context, info xbfeature.Info, body []byte) (int, []byte, error) {
	s.debug(info, pathHost, body)
	ses, err := xmatch.DecodeSession(body)
	if err != nil {
		return 400, nil, err
	}
	if ses.TitleID != DashboardTitle || (info.TitleID != 0 && info.TitleID != DashboardTitle) {
		return 501, nil, fmt.Errorf("title %08x (connection title %08x) is not served: only the dashboard's voice chat", ses.TitleID, info.TitleID)
	}
	if ses.SessionID.IsZero() || (isCreateFlags(ses.SessionID) && !s.known(ses.SessionID)) {
		return s.create(ctx, info, ses)
	}
	return s.update(ctx, info, ses)
}

func (s *Server) known(id xmatch.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	return s.byID[id] != nil
}

func (s *Server) create(ctx context.Context, info xbfeature.Info, ses xmatch.Session) (int, []byte, error) {
	// A host starting a new chat while an old one is registered replaces it.
	s.mu.Lock()
	s.init()
	old := s.byHost[info.CoreAccountID]
	s.mu.Unlock()
	if !old.IsZero() {
		s.end(ctx, old, "replaced")
	}

	id := newID()
	var key xmatch.Key
	if _, err := rand.Read(key[:]); err != nil {
		return 500, nil, err
	}
	resp, err := s.Voice.CreateSession(ctx, &voicev1.CreateSessionRequest{
		OwnerAccount: info.CoreAccountID, Kind: voicev1.SessionKind_SESSION_KIND_NATIVE, MaxParticipants: 16,
		EndWhenOwnerLeaves: true, Title: "xbox-dashboard", Transport: voicev1.Transport_TRANSPORT_NATIVE_PEER,
		NativeRef: hex.EncodeToString(id[:]),
	})
	if err != nil {
		return unavailable(err), nil, fmt.Errorf("voice service: %w", err)
	}
	now := s.now()
	r := &record{id: id, key: key, coreID: resp.GetSession().GetId(), host: info.CoreAccountID, hostXUID: info.XUID,
		title: ses.TitleID, addr: ses.Host, pub: [2]uint32{ses.PublicOpen, ses.PublicFilled}, priv: [2]uint32{ses.PrivateOpen, ses.PrivateFilled},
		attrs: ses.Attributes, created: now, touched: now, members: map[string]time.Time{}}
	s.mu.Lock()
	s.byID[id] = r
	s.byHost[r.host] = id
	s.mu.Unlock()
	log.Printf("svc6: voice chat created gen=%d session=%s open=%d/%d filled=%d/%d attrs=%d", info.Generation, hex.EncodeToString(id[:2])+"..", ses.PublicOpen, ses.PrivateOpen, ses.PublicFilled, ses.PrivateFilled, len(ses.Attributes))
	return 200, xmatch.Info{SessionID: id, Key: key}.Encode(), nil
}

func (s *Server) update(ctx context.Context, info xbfeature.Info, ses xmatch.Session) (int, []byte, error) {
	s.mu.Lock()
	s.init()
	r := s.byID[ses.SessionID]
	if r == nil {
		s.mu.Unlock()
		return 404, nil, errors.New("update of an unknown session")
	}
	if r.host != info.CoreAccountID || r.title != ses.TitleID {
		s.mu.Unlock()
		return 403, nil, errors.New("update by a console that is not the host")
	}
	if r.addr != ses.Host {
		s.mu.Unlock()
		return 409, nil, errors.New("the host address of a session cannot change")
	}
	r.pub, r.priv, r.attrs = [2]uint32{ses.PublicOpen, ses.PublicFilled}, [2]uint32{ses.PrivateOpen, ses.PrivateFilled}, ses.Attributes
	r.touched = s.now()
	coreID, key, id := r.coreID, r.key, r.id
	s.mu.Unlock()
	// The host is alive: renew its participant lease.
	if _, err := s.Voice.Heartbeat(ctx, &voicev1.HeartbeatRequest{SessionId: coreID, AccountId: info.CoreAccountID, LeaseSeconds: 900}); err != nil {
		log.Printf("svc6: host heartbeat: %v", err)
	}
	return 200, xmatch.Info{SessionID: id, Key: key}.Encode(), nil
}

func (s *Server) client(ctx context.Context, info xbfeature.Info, body []byte) (int, []byte, error) {
	s.debug(info, pathClient, body)
	q, err := xmatch.DecodeSearch(body)
	if err != nil {
		return 400, nil, err
	}
	if q.TitleID != DashboardTitle || (info.TitleID != 0 && info.TitleID != DashboardTitle) {
		return 501, nil, fmt.Errorf("search for title %08x is not served", q.TitleID)
	}
	id, ok := q.FindID()
	if !ok {
		return 501, nil, fmt.Errorf("search procedure %d is not served: only find by session id", q.ProcedureIndex)
	}
	empty := xmatch.EncodeResults(0, nil)

	s.mu.Lock()
	s.init()
	r := s.byID[id]
	var snap record
	if r != nil {
		snap = *r
	}
	s.mu.Unlock()
	if r == nil {
		return 200, empty, nil
	}

	lease := uint32(s.joinLease() / time.Second)
	if snap.host != info.CoreAccountID {
		_, err = s.Voice.JoinSession(ctx, &voicev1.JoinSessionRequest{SessionId: snap.coreID, AccountId: info.CoreAccountID,
			Transport: voicev1.Transport_TRANSPORT_NATIVE_PEER, LeaseSeconds: lease})
		switch status.Code(err) {
		case codes.OK:
		case codes.PermissionDenied, codes.ResourceExhausted:
			// Not allowed (not a friend, blocked, banned) or the chat is full: the
			// same answer as a chat that is gone. The reason stays in our log.
			log.Printf("svc6: lookup refused for gen=%d: %s", info.Generation, status.Convert(err).Message())
			return 200, empty, nil
		case codes.NotFound, codes.FailedPrecondition:
			s.forget(id)
			return 200, empty, nil
		default:
			return unavailable(err), nil, fmt.Errorf("voice service: %w", err)
		}
	}
	s.mu.Lock()
	if cur := s.byID[id]; cur == r {
		r.touched = s.now()
		r.members[info.CoreAccountID] = r.touched
	}
	s.mu.Unlock()
	res := xmatch.Result{SessionID: snap.id, Host: snap.addr, Key: snap.key,
		PublicOpen: snap.pub[0], PrivateOpen: snap.priv[0], PublicFilled: snap.pub[1], PrivateFilled: snap.priv[1], Attributes: snap.attrs}
	return 200, xmatch.EncodeResults(0, []xmatch.Result{res}), nil
}

func (s *Server) remove(ctx context.Context, info xbfeature.Info, body []byte) (int, []byte, error) {
	s.debug(info, pathDelete, body)
	d, err := xmatch.DecodeDelete(body)
	if err != nil {
		return 400, nil, err
	}
	s.mu.Lock()
	s.init()
	r := s.byID[d.SessionID]
	s.mu.Unlock()
	if r == nil {
		return 404, nil, nil
	}
	if r.host != info.CoreAccountID {
		return 403, nil, errors.New("delete by a console that is not the host")
	}
	s.end(ctx, d.SessionID, "deleted")
	return 200, nil, nil
}

func unavailable(err error) int {
	if status.Code(err) == codes.InvalidArgument {
		return 400
	}
	return 503
}

func (s *Server) forget(id xmatch.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.byID[id]; r != nil {
		delete(s.byID, id)
		if s.byHost[r.host] == id {
			delete(s.byHost, r.host)
		}
	}
}

// end drops a record and ends the session in the voice service.
func (s *Server) end(ctx context.Context, id xmatch.ID, why string) {
	s.mu.Lock()
	r := s.byID[id]
	s.mu.Unlock()
	if r == nil {
		return
	}
	s.forget(id)
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.Voice.EndSession(ectx, &voicev1.EndSessionRequest{SessionId: r.coreID, ActorAccount: r.host}); err != nil && status.Code(err) != codes.NotFound && status.Code(err) != codes.FailedPrecondition {
		log.Printf("svc6: end session (%s): %v", why, err)
	}
}

// expire ends records nobody has touched for TTL.
func (s *Server) expire(ctx context.Context) {
	now := s.now()
	var stale []xmatch.ID
	s.mu.Lock()
	s.init()
	for id, r := range s.byID {
		if now.Sub(r.touched) > s.ttl() {
			stale = append(stale, id)
		}
	}
	s.mu.Unlock()
	for _, id := range stale {
		s.end(ctx, id, "idle")
	}
}

// Run keeps hosted sessions alive in the voice service and expires idle ones
// until ctx ends. The voice service ends a session whose host lease lapses, so
// the host's lease is renewed here for as long as the record is resolvable.
func (s *Server) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.expire(ctx)
		type hb struct{ core, host string }
		var live []hb
		s.mu.Lock()
		for _, r := range s.byID {
			live = append(live, hb{r.coreID, r.host})
		}
		s.mu.Unlock()
		for _, h := range live {
			hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := s.Voice.Heartbeat(hctx, &voicev1.HeartbeatRequest{SessionId: h.core, AccountId: h.host, LeaseSeconds: 300})
			cancel()
			if err != nil && (status.Code(err) == codes.NotFound || status.Code(err) == codes.FailedPrecondition) {
				// The voice service no longer has it (restart, kick): forget ours.
				s.mu.Lock()
				for id, r := range s.byID {
					if r.coreID == h.core {
						delete(s.byID, id)
						delete(s.byHost, r.host)
					}
				}
				s.mu.Unlock()
			}
		}
	}
}

// Sessions returns how many sessions are resolvable (metrics, tests).
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}
