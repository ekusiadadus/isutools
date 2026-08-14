package multihost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type HubPeerConfig struct {
	Name                 string
	Endpoint             string
	Token                string
	Required             bool
	RequiredSections     []string
	RequiredCapabilities []string
}

const peerControlRequestTimeout = 2 * time.Second

type HubConfig struct {
	Peers   []HubPeerConfig
	Client  *http.Client
	Now     func() time.Time
	Preempt bool
}

type Hub struct {
	peers   []hubPeer
	client  *http.Client
	now     func() time.Time
	preempt bool
}

type hubPeer struct {
	config   HubPeerConfig
	endpoint *url.URL
}

type HubRun struct {
	RunID       string
	Nonce       string
	Validity    string
	Peers       []PeerResult
	hub         *Hub
	leaseCancel context.CancelFunc
	leaseDone   chan struct{}
}

func NewHub(config HubConfig) (*Hub, error) {
	if len(config.Peers) == 0 || len(config.Peers) > MaxPeers {
		return nil, fmt.Errorf("multihost: peer count outside 1..%d", MaxPeers)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	seen := map[string]struct{}{}
	peers := make([]hubPeer, 0, len(config.Peers))
	for _, item := range config.Peers {
		if item.Name == "" || len(item.Token) < 32 {
			return nil, errors.New("multihost: invalid peer config")
		}
		parsed, err := url.Parse(item.Endpoint)
		if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
			return nil, errors.New("multihost: peer endpoint must be an http origin")
		}
		host := net.ParseIP(parsed.Hostname())
		if host == nil || !host.IsLoopback() {
			return nil, errors.New("multihost: peer endpoint must use a literal loopback IP")
		}
		key := parsed.String()
		if _, ok := seen[key]; ok {
			return nil, errors.New("multihost: duplicate peer endpoint")
		}
		seen[key] = struct{}{}
		item.RequiredSections = sortedUnique(item.RequiredSections)
		item.RequiredCapabilities = sortedUnique(item.RequiredCapabilities)
		peers = append(peers, hubPeer{config: item, endpoint: parsed})
	}
	client := config.Client
	if client == nil {
		transport := &http.Transport{Proxy: nil, DisableCompression: true, MaxResponseHeaderBytes: 8 << 10, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}
		client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Hub{peers: peers, client: client, now: config.Now, preempt: config.Preempt}, nil
}

func (h *Hub) Preflight(ctx context.Context) []PeerResult {
	results := make([]PeerResult, len(h.peers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, peer := range h.peers {
		i, peer := i, peer
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = h.preflightPeer(ctx, peer)
		}()
	}
	wg.Wait()
	seen := map[string]int{}
	for i := range results {
		if results[i].Failure != nil {
			continue
		}
		id := results[i].Info.AgentID
		if prior, ok := seen[id]; ok {
			results[i].Failure = failure("preflight", "duplicate-agent", h.now())
			if results[prior].Failure == nil {
				results[prior].Failure = failure("preflight", "duplicate-agent", h.now())
			}
		} else {
			seen[id] = i
		}
	}
	return results
}

func (h *Hub) preflightPeer(ctx context.Context, peer hubPeer) PeerResult {
	result := PeerResult{Name: peer.config.Name, Required: peer.config.Required, Sealed: "", Form: ""}
	var info PeerInfoDTO
	status, err := h.do(ctx, peer, http.MethodGet, "/peer/info", nil, &info, 256<<10)
	if err != nil || status != http.StatusOK {
		result.Failure = failure("preflight", statusCode(err, status), h.now())
		return result
	}
	result.Info = info
	result.Form = info.Form
	switch {
	case info.ProtocolVersion != ProtocolVersion:
		result.Failure = failure("preflight", "protocol-mismatch", h.now())
	case info.SchemaVersion > SchemaVersion:
		result.Failure = failure("preflight", "schema-incompatible", h.now())
	case !containsAll(info.Capabilities, peer.config.RequiredCapabilities):
		result.Failure = failure("preflight", "capability-missing", h.now())
	case !containsAll(info.Sections, peer.config.RequiredSections):
		result.Failure = failure("preflight", "section-missing", h.now())
	}
	return result
}

func (h *Hub) Start(ctx context.Context, runID, nonce string) (*HubRun, error) {
	if !validID(runID) || !validID(nonce) {
		return nil, errors.New("multihost: invalid run identity")
	}
	results := h.Preflight(ctx)
	run := &HubRun{RunID: runID, Nonce: nonce, Validity: "valid", Peers: results, hub: h}
	for i := range results {
		if results[i].Failure != nil {
			if results[i].Required {
				run.Validity = "invalid"
			} else if run.Validity == "valid" {
				run.Validity = "partial"
			}
		}
	}
	if run.Validity == "invalid" {
		return run, errors.New("multihost: required peer preflight failed")
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range run.Peers {
		if run.Peers[i].Failure != nil {
			continue
		}
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			peer := h.peers[i]
			req := StartRunRequest{RunID: &runID, Nonce: &nonce, Preempt: &h.preempt}
			sent := h.now().UTC()
			var dto StartResultDTO
			status, err := h.do(ctx, peer, http.MethodPost, "/peer/runs", req, &dto, 256<<10)
			ack := h.now().UTC()
			run.Peers[i].StartSendAck = [2]time.Time{sent, ack}
			if err != nil || status != http.StatusCreated && status != http.StatusOK {
				run.Peers[i].Failure = failure("start", statusCode(err, status), ack)
				return
			}
			run.Peers[i].Start = &dto
		}()
	}
	wg.Wait()
	for i := range run.Peers {
		if run.Peers[i].Failure != nil {
			if run.Peers[i].Required {
				run.Validity = "invalid"
			} else if run.Validity == "valid" {
				run.Validity = "partial"
			}
		}
	}
	if run.Validity == "invalid" {
		h.abortAll(context.Background(), run)
		return run, errors.New("multihost: required peer start failed")
	}
	leaseCtx, cancel := context.WithCancel(context.Background())
	run.leaseCancel = cancel
	run.leaseDone = make(chan struct{})
	go func() {
		defer close(run.leaseDone)
		h.renewLeases(leaseCtx, run)
	}()
	return run, nil
}

func (h *Hub) Finish(ctx context.Context, run *HubRun, hubBytes int64) ([]PeerResult, string) {
	if run == nil || run.hub != h {
		return nil, "invalid"
	}
	if run.leaseCancel != nil {
		run.leaseCancel()
		if run.leaseDone != nil {
			select {
			case <-run.leaseDone:
			case <-ctx.Done():
				return run.Peers, "invalid"
			}
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range run.Peers {
		if run.Peers[i].Start == nil {
			continue
		}
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			peer := h.peers[i]
			sent := h.now().UTC()
			var dto FinishAcceptedDTO
			status, err := h.do(ctx, peer, http.MethodPost, "/peer/runs/"+url.PathEscape(run.RunID)+"/finish", FinishRunRequest{}, &dto, 256<<10)
			ack := h.now().UTC()
			run.Peers[i].FinishSendAck = [2]time.Time{sent, ack}
			if err != nil || status != http.StatusOK {
				run.Peers[i].Failure = failure("finish", statusCode(err, status), ack)
				return
			}
			run.Peers[i].Finish = &dto
		}()
	}
	wg.Wait()
	var pollWG sync.WaitGroup
	for i := range run.Peers {
		if run.Peers[i].Start == nil || run.Peers[i].Finish == nil {
			continue
		}
		i := i
		pollWG.Add(1)
		go func() {
			defer pollWG.Done()
			h.pollPeer(ctx, &run.Peers[i], h.peers[i], run.RunID)
		}()
	}
	pollWG.Wait()
	hubBudgetInvalid := hubBytes < 0 || hubBytes > HubSelfReserve
	remaining := int64(TotalSnapshotCap) - hubBytes
	if remaining < 0 {
		remaining = 0
	}
	active := 0
	for i := range run.Peers {
		if run.Peers[i].Status != nil && run.Peers[i].Status.SnapshotReady {
			active++
		}
	}
	budget := int64(PerPeerDefaultBytes)
	if active > 0 && remaining/int64(active) < budget {
		budget = remaining / int64(active)
	}
	var fetchWG sync.WaitGroup
	for i := range run.Peers {
		if run.Peers[i].Status == nil || !run.Peers[i].Status.SnapshotReady {
			continue
		}
		if hubBudgetInvalid || budget < MinPeerBytes {
			run.Peers[i].Failure = failure("fetch", "budget-exhausted", h.now())
			continue
		}
		i := i
		fetchWG.Add(1)
		go func() {
			defer fetchWG.Done()
			h.fetchPeer(ctx, &run.Peers[i], h.peers[i], run.RunID, budget)
		}()
	}
	fetchWG.Wait()
	validity := run.Validity
	if hubBudgetInvalid {
		validity = "invalid"
	}
	for i := range run.Peers {
		peer := &run.Peers[i]
		if peer.Failure != nil {
			if peer.Required {
				validity = "invalid"
			} else if validity == "valid" {
				validity = "partial"
			}
		}
	}
	h.ackAll(context.Background(), run)
	run.Validity = validity
	return run.Peers, validity
}

// Abort stops lease renewal and seals every started participant with abort.
// It is idempotent at the peer protocol layer.
func (h *Hub) Abort(ctx context.Context, run *HubRun) ([]PeerResult, string) {
	if run == nil || run.hub != h {
		return nil, "invalid"
	}
	if run.leaseCancel != nil {
		run.leaseCancel()
		if run.leaseDone != nil {
			select {
			case <-run.leaseDone:
			case <-ctx.Done():
				return run.Peers, "invalid"
			}
		}
	}
	h.abortAll(ctx, run)
	run.Validity = "invalid"
	return run.Peers, run.Validity
}

func (h *Hub) pollPeer(ctx context.Context, result *PeerResult, peer hubPeer, runID string) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var dto RunStatusDTO
		status, err := h.do(ctx, peer, http.MethodGet, "/peer/runs/"+url.PathEscape(runID), nil, &dto, 256<<10)
		if err != nil || status != http.StatusOK {
			result.Failure = failure("poll", statusCode(err, status), h.now())
			return
		}
		result.Status = &dto
		if dto.State == "aborted" || dto.State == "expired" {
			result.Failure = failure("poll", "aborted-by-peer", h.now())
			return
		}
		if dto.SnapshotReady {
			return
		}
		select {
		case <-ctx.Done():
			result.Failure = failure("poll", "timeout", h.now())
			return
		case <-ticker.C:
		}
	}
}
func (h *Hub) fetchPeer(ctx context.Context, result *PeerResult, peer hubPeer, runID string, budget int64) {
	var snapshot LocalSnapshot
	path := fmt.Sprintf("/peer/runs/%s/snapshot?max_bytes=%d", url.PathEscape(runID), budget)
	status, err := h.do(ctx, peer, http.MethodGet, path, nil, &snapshot, budget+(1<<20))
	if err != nil || status != http.StatusOK {
		result.Failure = failure("fetch", statusCode(err, status), h.now())
		return
	}
	if snapshot.RunID != runID || snapshot.SchemaVersion > SchemaVersion {
		result.Failure = failure("validate", "run-id-mismatch", h.now())
		return
	}
	result.Local = &snapshot
	result.Issues = append(result.Issues, snapshot.Issues...)
}

func (h *Hub) renewLeases(ctx context.Context, run *HubRun) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var wg sync.WaitGroup
			for i := range run.Peers {
				if run.Peers[i].Start == nil {
					continue
				}
				i := i
				wg.Add(1)
				go func() {
					defer wg.Done()
					requestCtx, cancel := context.WithTimeout(ctx, peerControlRequestTimeout)
					defer cancel()
					var dto LeaseDTO
					_, _ = h.do(requestCtx, h.peers[i], http.MethodPost, "/peer/runs/"+url.PathEscape(run.RunID)+"/lease", struct{}{}, &dto, 64<<10)
				}()
			}
			wg.Wait()
		}
	}
}
func (h *Hub) abortAll(ctx context.Context, run *HubRun) {
	var wg sync.WaitGroup
	for i := range run.Peers {
		if run.Peers[i].Start == nil {
			continue
		}
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			requestCtx, cancel := context.WithTimeout(ctx, peerControlRequestTimeout)
			defer cancel()
			var dto AbortResultDTO
			status, err := h.do(requestCtx, h.peers[i], http.MethodPost, "/peer/runs/"+url.PathEscape(run.RunID)+"/abort", AbortRequest{Reason: "hub start barrier failed"}, &dto, 256<<10)
			if err == nil && status == http.StatusOK {
				run.Peers[i].Aborted = &dto
				run.Peers[i].Sealed = "abort"
			} else {
				run.Peers[i].Sealed = "failed"
			}
		}()
	}
	wg.Wait()
}
func (h *Hub) ackAll(ctx context.Context, run *HubRun) {
	var wg sync.WaitGroup
	for i := range run.Peers {
		if run.Peers[i].Start == nil {
			continue
		}
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			requestCtx, cancel := context.WithTimeout(ctx, peerControlRequestTimeout)
			defer cancel()
			status, err := h.do(requestCtx, h.peers[i], http.MethodPost, "/peer/runs/"+url.PathEscape(run.RunID)+"/ack", struct{}{}, nil, 64<<10)
			if err == nil && status == http.StatusNoContent {
				run.Peers[i].Sealed = "ack"
			} else {
				run.Peers[i].Sealed = "failed"
			}
		}()
	}
	wg.Wait()
}

func (h *Hub) do(ctx context.Context, peer hubPeer, method, path string, input, output any, limit int64) (int, error) {
	endpoint := *peer.endpoint
	endpoint.Path = path
	if at := strings.Index(path, "?"); at >= 0 {
		endpoint.Path = path[:at]
		endpoint.RawQuery = path[at+1:]
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+peer.config.Token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return response.StatusCode, err
	}
	if int64(len(data)) > limit {
		return response.StatusCode, errors.New("response-size-exceeded")
	}
	if output != nil && response.StatusCode >= 200 && response.StatusCode < 300 && response.StatusCode != http.StatusNoContent {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(output); err != nil {
			return response.StatusCode, errors.New("malformed-response")
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return response.StatusCode, errors.New("malformed-response")
		}
	}
	return response.StatusCode, nil
}

func failure(phase, code string, at time.Time) *ParticipantFailureDTO {
	return &ParticipantFailureDTO{Phase: phase, Code: code, At: at.UTC()}
}
func statusCode(err error, status int) string {
	if err != nil {
		if strings.Contains(err.Error(), "malformed") {
			return "malformed"
		}
		if strings.Contains(err.Error(), "size") {
			return "size-exceeded"
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return "timeout"
		}
		return "unreachable"
	}
	if status < 200 || status >= 300 {
		return "http-status"
	}
	return "internal"
}
func containsAll(values, wants []string) bool {
	set := map[string]struct{}{}
	for _, v := range values {
		set[v] = struct{}{}
	}
	for _, v := range wants {
		if _, ok := set[v]; !ok {
			return false
		}
	}
	return true
}
