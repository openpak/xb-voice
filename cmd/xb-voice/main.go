// SPDX-License-Identifier: AGPL-3.0-only

// xb-voice: the original Xbox adapter of the neutral voice service. It serves
// native service 6 (xmatch) to the gateway for the dashboard's voice chat
// sessions and mirrors them into the voice service (github.com/openpak/voice).
// The gateway reaches it with XBGW_BACKENDS=6=<host>:<XB_FEATURE port> over
// mutual TLS (gateway identity spiffe://openpak/xb-gateway).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
	"github.com/openpak/xb-common/xbfeature"
	"github.com/openpak/xb-voice/internal/svc6"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func required(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s is required", name)
	}
	return v
}

func pool(file string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid CA bundle")
	}
	return p, nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	clientCA, err := pool(required("XB_CLIENT_CA_FILE"))
	if err != nil {
		return fmt.Errorf("client CA: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(required("XB_TLS_CERT_FILE"), required("XB_TLS_KEY_FILE"))
	if err != nil {
		return errors.New("cannot load workload certificate")
	}
	keyBytes, err := os.ReadFile(required("XB_VOICE_KEY_FILE"))
	if err != nil || strings.TrimSpace(string(keyBytes)) == "" {
		return errors.New("cannot read the voice service key")
	}
	key := strings.TrimSpace(string(keyBytes))

	var transport credentials.TransportCredentials
	if ca := os.Getenv("XB_VOICE_CA_FILE"); ca != "" {
		if transport, err = credentials.NewClientTLSFromFile(ca, os.Getenv("XB_VOICE_SERVER_NAME")); err != nil {
			return errors.New("invalid voice service TLS configuration")
		}
	} else if os.Getenv("XB_VOICE_PLAINTEXT_INTERNAL") == "1" {
		transport = insecure.NewCredentials()
	} else {
		return errors.New("voice service TLS required; set XB_VOICE_PLAINTEXT_INTERNAL=1 only on the private container network")
	}
	conn, err := grpc.NewClient(required("XB_VOICE_ADDR"), grpc.WithTransportCredentials(transport),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return inv(metadata.AppendToOutgoingContext(ctx, "x-internal-key", key), method, req, reply, cc, opts...)
		}))
	if err != nil {
		return errors.New("cannot configure the voice service client")
	}
	defer conn.Close()

	srv := &svc6.Server{Voice: voicev1.NewVoiceClient(conn), DebugRequests: os.Getenv("XB_VOICE_DEBUG_REQUESTS") == "1"}
	if v := os.Getenv("XB_VOICE_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			return errors.New("XB_VOICE_SESSION_TTL must be a duration of at least 1m")
		}
		srv.TTL = d
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go srv.Run(ctx, 30*time.Second)

	lis, err := net.Listen("tcp", required("XB_FEATURE_ADDR"))
	if err != nil {
		return fmt.Errorf("feature listener: %w", err)
	}
	g := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: clientCA,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13})), grpc.MaxRecvMsgSize(256<<10))
	xbfeature.Register(g, xbfeature.LogHTTP(srv))
	go func() {
		if err := g.Serve(lis); err != nil {
			log.Printf("feature server: %v", err)
			stop()
		}
	}()

	// Health on loopback only (the container's own probe).
	hs := &http.Server{Addr: required("XB_LISTEN_ADDR"), ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			fmt.Fprintf(w, "ok sessions=%d\n", srv.Sessions())
			return
		}
		http.NotFound(w, r)
	})}
	go func() {
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health: %v", err)
		}
	}()
	<-ctx.Done()
	g.GracefulStop()
	sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(sd)
}
