// Package hubconfig loads the secret-bearing multi-host peer list.
package hubconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/ekusiadadus/isutools/multihost"
)

const MaxBytes = 64 << 10

var ErrRejected = errors.New("hub config: rejected")

type peerFile struct {
	Name                 string   `json:"name"`
	Endpoint             string   `json:"endpoint"`
	Token                string   `json:"token"`
	Required             bool     `json:"required"`
	RequiredSections     []string `json:"required_sections,omitempty"`
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
}

// Load accepts only an owner-only regular file and returns protocol config.
// Errors never include file contents, endpoints, or bearer tokens.
func Load(path string) ([]multihost.HubPeerConfig, error) {
	info, err := os.Lstat(path)
	if err != nil || !safeInfo(info) {
		return nil, ErrRejected
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrRejected
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !safeInfo(after) || !os.SameFile(info, after) {
		return nil, ErrRejected
	}
	body, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil || len(body) > MaxBytes {
		return nil, ErrRejected
	}
	var raw []peerFile
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, ErrRejected
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, ErrRejected
	}
	if len(raw) == 0 || len(raw) > multihost.MaxPeers {
		return nil, ErrRejected
	}
	result := make([]multihost.HubPeerConfig, len(raw))
	for i, peer := range raw {
		if peer.Name == "" || peer.Endpoint == "" || len(peer.Token) < 32 {
			return nil, fmt.Errorf("%w: peer %d is incomplete", ErrRejected, i)
		}
		result[i] = multihost.HubPeerConfig{Name: peer.Name, Endpoint: peer.Endpoint, Token: peer.Token, Required: peer.Required, RequiredSections: peer.RequiredSections, RequiredCapabilities: peer.RequiredCapabilities}
	}
	return result, nil
}

func safeInfo(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > MaxBytes {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
