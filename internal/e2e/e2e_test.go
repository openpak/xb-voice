// SPDX-License-Identifier: AGPL-3.0-only

// Package e2e drives the dashboard's voice chat through the real pieces that
// exist: console requests enter through a real mutual-TLS xbfeature stream (as
// from the gateway) into svc6, which mirrors the session into the real voice
// service (in process, with a stand-in account core). It also checks the point
// of the two-layer design: an app-side adapter sees the Xbox session and can join
// it, with no Xbox knowledge. The console side is a test client written against
// the same wire structures; no retail console has run this.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
	"github.com/openpak/voice/testkit"
	"github.com/openpak/xb-common/xbfeature"
	"github.com/openpak/xb-voice/internal/svc6"
	"github.com/openpak/xb-voice/internal/xmatch"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type pki struct {
	ca    *x509.Certificate
	key   *ecdsa.PrivateKey
	pool  *x509.CertPool
	count int64
}

func newPKI() *pki {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &pki{ca: ca, key: k, pool: pool}
}

func (p *pki) leaf(uri string) tls.Certificate {
	p.count++
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse(uri)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(p.count + 1), Subject: pkix.Name{CommonName: "leaf"}, URIs: []*url.URL{u},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, p.ca, &k.PublicKey, p.key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

type world struct {
	t    *testing.T
	svc  *testkit.Service
	core *testkit.Core
	srv  *svc6.Server
	cc   *grpc.ClientConn
}

func newWorld(t *testing.T) *world {
	t.Helper()
	svc := testkit.NewService(t)
	w := &world{t: t, svc: svc, core: svc.Core}
	w.srv = &svc6.Server{Voice: svc.Client(testkit.XboxKey)}

	p := newPKI()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{p.leaf("spiffe://openpak/xb-voice")},
		ClientCAs: p.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13})))
	xbfeature.Register(g, xbfeature.LogHTTP(w.srv))
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	w.cc, err = grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{p.leaf(xbfeature.GatewayID)}, RootCAs: p.pool, ServerName: "localhost", MinVersion: tls.VersionTLS13})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.cc.Close() })
	return w
}

// xmatch sends one request as the console of (account, xuid) would, through a
// gateway-style stream, and returns the HTTP status and the body.
func (w *world) xmatch(account string, xuid uint64, path string, body []byte) (int, []byte) {
	w.t.Helper()
	got := make(chan []byte, 16)
	closed := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := xbfeature.Open(ctx, w.cc, xbfeature.Info{PrincipalID: fmt.Sprintf("%016x", xuid), CoreAccountID: account, XUID: xuid, ServiceID: 6, Generation: 1},
		func(b []byte) error { got <- append([]byte(nil), b...); return nil }, func() { close(closed) })
	if err != nil {
		w.t.Fatalf("open stream: %v", err)
	}
	req := fmt.Sprintf("POST %s HTTP/1.0\r\nUser-Agent: 1/1.0\r\nContent-Type: xon/6\r\nContent-Length: %d\r\n\r\n", path, len(body))
	if err := st.Send(append([]byte(req), body...)); err != nil {
		w.t.Fatal(err)
	}
	st.CloseSend()
	var raw bytes.Buffer
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case b := <-got:
			raw.Write(b)
		case <-closed:
			done = true
		case <-deadline:
			w.t.Fatalf("no reply to %s", path)
		}
	}
	for len(got) > 0 {
		raw.Write(<-got)
	}
	resp, err := http.ReadResponse(bufio.NewReader(&raw), nil)
	if err != nil {
		w.t.Fatalf("response: %v (%q)", err, raw.String())
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 && resp.Header.Get("Content-Type") != "xon/6" {
		w.t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	return resp.StatusCode, b
}

func hostAddr() xmatch.Addr {
	var a xmatch.Addr
	copy(a[:], []byte{10, 0, 2, 15, 203, 0, 113, 9, 0x90, 0x73, 0, 0x50, 0xf2, 1, 2, 3, 0xAA, 0xBB, 0xCC})
	return a
}

const (
	xAlice = uint64(0x0009000000000001)
	xBob   = uint64(0x0009000000000002)
	xEve   = uint64(0x0009000000000003)
)

func TestVoiceChatThroughTheRealStream(t *testing.T) {
	w := newWorld(t)
	w.core.Befriend("alice", "bob")

	code, body := w.xmatch("alice", xAlice, "/xmatch/xmatchhost.srf",
		xmatch.Session{TitleID: svc6.DashboardTitle, Host: hostAddr(), PrivateOpen: 15, PublicFilled: 1, PrivateFilled: 1}.Encode())
	if code != 200 {
		t.Fatalf("create: %d", code)
	}
	info, err := xmatch.DecodeInfo(body)
	if err != nil {
		t.Fatal(err)
	}
	find := xmatch.FindRequest(svc6.DashboardTitle, xmatch.Addr{}, info.SessionID).Encode()

	// The invited friend gets the host's address and the key.
	code, body = w.xmatch("bob", xBob, "/xmatch/xmatchclient.srf", find)
	res, err := xmatch.DecodeResults(body)
	if code != 200 || err != nil || len(res) != 1 || res[0].Key != info.Key || res[0].Host != hostAddr() {
		t.Fatalf("friend lookup: %d %v %v", code, res, err)
	}
	// A stranger learns nothing, and neither does a blocked account.
	code, body = w.xmatch("eve", xEve, "/xmatch/xmatchclient.srf", find)
	if res, _ = xmatch.DecodeResults(body); code != 200 || len(res) != 0 || bytes.Contains(body, info.Key[:]) {
		t.Fatalf("stranger: %d %v", code, res)
	}
	w.core.Befriend("alice", "eve")
	w.core.Block("bob", "eve") // eve is the host's friend but bob blocked her: bob is in the chat
	code, body = w.xmatch("eve", xEve, "/xmatch/xmatchclient.srf", find)
	if res, _ = xmatch.DecodeResults(body); code != 200 || len(res) != 0 {
		t.Fatalf("blocked by a member: %d %v", code, res)
	}
}

// The voice service holds the Xbox session as an ordinary session: this is what
// lets another family's adapter see it and join it (the two-layer design). Here
// the app adapter, with no Xbox knowledge, finds the session by the Xbox native
// reference, sees the console participants, and a phone user joins.
func TestAnotherAdapterSeesAndJoinsTheXboxSession(t *testing.T) {
	w := newWorld(t)
	w.core.Befriend("alice", "bob")
	w.core.Befriend("alice", "carol")

	code, reply := w.xmatch("alice", xAlice, "/xmatch/xmatchhost.srf",
		xmatch.Session{TitleID: svc6.DashboardTitle, Host: hostAddr(), PrivateOpen: 15, PublicFilled: 1, PrivateFilled: 1}.Encode())
	if code != 200 {
		t.Fatalf("create: %d", code)
	}
	info, _ := xmatch.DecodeInfo(reply)
	if code, _ = w.xmatch("bob", xBob, "/xmatch/xmatchclient.srf", xmatch.FindRequest(svc6.DashboardTitle, xmatch.Addr{}, info.SessionID).Encode()); code != 200 {
		t.Fatalf("lookup: %d", code)
	}

	ctx := context.Background()
	app := w.svc.Client(testkit.AppKey)
	got, err := app.GetSessionByNativeRef(ctx, &voicev1.GetSessionByNativeRefRequest{Namespace: "xbox", NativeRef: hex.EncodeToString(info.SessionID[:])})
	if err != nil {
		t.Fatalf("native ref lookup: %v", err)
	}
	s := got.GetSession()
	if s.GetOwnerAccount() != "alice" || s.GetKind() != voicev1.SessionKind_SESSION_KIND_NATIVE || len(s.GetParticipants()) != 2 {
		t.Fatalf("session: %v", s)
	}
	for _, p := range s.GetParticipants() {
		if p.GetAdapter() != "xbox" || p.GetTransport() != voicev1.Transport_TRANSPORT_NATIVE_PEER {
			t.Fatalf("console participants are native peers of the xbox adapter: %v", p)
		}
	}
	// The key never leaves the Xbox adapter.
	if bytes.Contains([]byte(s.String()), []byte(hex.EncodeToString(info.Key[:]))) {
		t.Fatal("the session key must not be in the voice service")
	}

	// carol, on her phone, joins the same session through the app adapter; the
	// account core decides, exactly as for a console.
	if _, err := app.JoinSession(ctx, &voicev1.JoinSessionRequest{SessionId: s.GetId(), AccountId: "carol", Transport: voicev1.Transport_TRANSPORT_WEBRTC,
		Codecs: []*voicev1.Codec{{Name: "opus", SampleRate: 48000, CanDecode: true, CanEncode: true}}}); err != nil {
		t.Fatalf("phone join: %v", err)
	}
	if _, err := app.JoinSession(ctx, &voicev1.JoinSessionRequest{SessionId: s.GetId(), AccountId: "mallory"}); err == nil {
		t.Fatal("a stranger on the phone must be refused like a stranger on a console")
	}
	// The host leaving ends the chat for the phone user as well (the Xbox host model).
	if code, _ = w.xmatch("alice", xAlice, "/xmatch/xmatchhostdelete.srf", xmatch.Delete{SessionID: info.SessionID, TitleID: svc6.DashboardTitle}.Encode()); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	after, err := app.GetSession(ctx, &voicev1.GetSessionRequest{SessionId: s.GetId()})
	if err != nil || after.GetSession().GetEndedUnix() == 0 {
		t.Fatalf("the session ended for everyone: %v %v", after, err)
	}
}
