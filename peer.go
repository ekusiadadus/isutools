package isutools

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/multihost"
	"github.com/ekusiadadus/isutools/sqlstats"
)

const (
	EnvPeer      = "ISUTOOLS_PEER"
	EnvPeerToken = "ISUTOOLS_PEER_TOKEN"
)

var ErrPeerListenerNotLoopback = errors.New("isutools: peer listener must use a literal loopback address")

type PeerOptions struct {
	Token     string
	Role      string
	MaxBytes  int64
	AccessLog string
}

var processPeerID = sync.OnceValue(newPeerAgentID)

// PeerHandler exposes the singleton run controller to a loopback-only peer
// listener. It intentionally does not create a second measurement lifecycle.
func PeerHandler(options PeerOptions) (http.Handler, error) {
	if Off() || !peerEnabled(os.Getenv(EnvPeer)) {
		return nil, multihost.ErrPeerDisabled
	}
	if options.Token == "" {
		options.Token = os.Getenv(EnvPeerToken)
	}
	if options.Role == "" {
		options.Role = "app"
	}
	core := defaultMeasurement()
	sections := core.ctrl.RegisteredCollectors()
	sections = append(sections, "connections", "dbinspect", "advisor-static")
	if core.explain != nil {
		sections = append(sections, "queryplan")
	}
	targets := make([]multihost.TargetSummaryDTO, 0)
	for _, target := range sqlstats.Targets() {
		purposes := make([]string, len(target.Purposes))
		for i, purpose := range target.Purposes {
			purposes[i] = string(purpose)
		}
		targets = append(targets, multihost.TargetSummaryDTO{ID: target.ID, Driver: target.Driver, Display: target.Display, Schema: target.Schema, Purposes: purposes})
	}
	peer, err := multihost.NewPeer(multihost.PeerOptions{
		Enabled: true, Token: options.Token, Role: options.Role, Form: "embedded",
		AgentID: processPeerID(), MaxBytes: options.MaxBytes, Sections: sections,
		Capabilities: []string{"run-v1", "strict-dto", "lease-v1", "bounded-snapshot"},
		Targets:      targets, Controller: core.ctrl,
		Snapshot: func(snapshot *runctl.Snapshot) map[string]any { return snapshot.Sections },
	})
	if err != nil {
		return nil, err
	}
	return peer, nil
}

func peerEnabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

// ServePeer serves PeerHandler on a literal loopback listener until ctx ends.
func ServePeer(ctx context.Context, addr string, options PeerOptions) error {
	if !literalLoopbackAddr(addr) {
		return ErrPeerListenerNotLoopback
	}
	handler, err := PeerHandler(options)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, MaxHeaderBytes: 8 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		<-done
		return ctx.Err()
	}
}

func literalLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func newPeerAgentID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}
