package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/internal/profilemodel"
	"github.com/ekusiadadus/isutools/internal/safefs"
)

const (
	profileAnalysisRendererVersion = "1"
	maxAnalysisEnvelopeBytes       = profilemodel.MaxAnalysisBodyBytes + 4<<10
	maxDerivedHTMLBytes            = 36 << 20
	maxAnalysisHTMLIncrement       = 4 << 20
	maxCommitFiles                 = 1024
	profileAnalysisHistory         = 3
)

const profileAnalysisTemplateText = `<section id="isutools-profile-analysis">
<h2>Profile Analysis</h2>
<p>status: {{.Status}} · analysis: <code>{{.AnalysisID}}</code></p>
<p>snapshot: <code>{{.SnapshotSHA256}}</code> · binary: {{.Binary.Match}} · worker: {{.Analyzer.Isolation.Mode}} / {{.Analyzer.Isolation.Bootstrap}}</p>
{{if .Diagnostics}}<h3>Analysis diagnostics</h3><ul>{{range .Diagnostics}}<li><strong>{{.Level}} / {{.Code}}</strong>: {{.Message}}</li>{{end}}</ul>{{end}}
{{range .Attempts}}<article class="isutools-profile-attempt">
<h3>{{.Kind}} ({{.Mode}} / {{.Status}})</h3>
<p>coverage: complete={{.Coverage.Complete}} · run={{.Coverage.RunSpanNs}}ns · capture={{.Coverage.CaptureSpanNs}}ns · head loss={{.Coverage.HeadLossNs}}ns · tail excess={{.Coverage.TailExcessNs}}ns · tail loss={{.Coverage.TailLossNs}}ns · stop={{.Coverage.StopReason}}</p>
{{if .Diagnostics}}<h4>Diagnostics</h4><ul>{{range .Diagnostics}}<li><strong>{{.Level}} / {{.Code}}</strong>: {{.Message}}</li>{{end}}</ul>{{end}}
<h4>Inputs</h4><ul>{{range .ExpectedInputs}}<li>expected {{.Point}}: <code>{{.File}}</code></li>{{end}}{{range .ObservedInputs}}<li>observed: <a href="/files/{{.File}}">{{.File}}</a> · {{.Bytes}} bytes · <code>{{.SHA256}}</code>{{if .Sidecar}}<br>completion: <a href="/files/{{.Sidecar}}">{{.Sidecar}}</a> · <code>{{.SidecarSHA256}}</code>{{end}}{{if .CoverageFile}}<br>coverage: <a href="/files/{{.CoverageFile}}">{{.CoverageFile}}</a> · <code>{{.CoverageSHA256}}</code>{{end}}</li>{{end}}</ul>
{{range .Summaries}}{{$denominator := .PercentDenominator}}<h4>{{.SampleType}} / {{.Unit}}</h4>
<p>net {{.NetTotal}} · positive {{.PositiveTotal}} · negative {{.NegativeMagnitude}} · denominator {{.PercentDenominator}} ({{.DenominatorMode}})</p>
{{if .Labels}}<h5>Labels</h5>{{range .Labels}}<table><caption>{{.Key}}</caption><thead><tr><th>value</th><th>total</th></tr></thead><tbody>{{range .Values}}<tr><td>{{.Value}}</td><td>{{.Total}}</td></tr>{{end}}</tbody></table>{{end}}{{end}}
{{range .Reports}}<h5>{{.Granularity}}</h5>
{{if .TopFlat}}<h6>Top flat</h6>{{template "profile-nodes" (profileRows .TopFlat $denominator)}}{{end}}
{{if .TopCumulative}}<h6>Top cumulative</h6>{{template "profile-nodes" (profileRows .TopCumulative $denominator)}}{{end}}
{{if .TopNegativeFlat}}<h6>Top negative flat</h6>{{template "profile-nodes" (profileRows .TopNegativeFlat $denominator)}}{{end}}
{{if .TopNegativeCumulative}}<h6>Top negative cumulative</h6>{{template "profile-nodes" (profileRows .TopNegativeCumulative $denominator)}}{{end}}
{{end}}{{end}}</article>{{end}}
</section>
{{define "profile-nodes"}}<table><thead><tr><th>function</th><th>file</th><th>line</th><th>value</th><th>%</th></tr></thead><tbody>{{range .}}<tr><td>{{.Node.Function}}</td><td>{{.Node.File}}</td><td>{{.Node.Line}}</td><td>{{.Node.Value}}</td><td>{{.Percent}}</td></tr>{{end}}</tbody></table>{{end}}`

type profileTemplateRow struct {
	Node    profilemodel.ProfileNode
	Percent string
}

func profileRows(nodes []profilemodel.ProfileNode, denominator int64) []profileTemplateRow {
	rows := make([]profileTemplateRow, len(nodes))
	for index, node := range nodes {
		rows[index] = profileTemplateRow{Node: node, Percent: profilePercent(node.Value, denominator)}
	}
	return rows
}

func profilePercent(value, denominator int64) string {
	if denominator <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", float64(value)*100/float64(denominator))
}

var profileAnalysisTemplate = template.Must(template.New("profile-analysis").Funcs(template.FuncMap{"profileRows": profileRows}).Parse(profileAnalysisTemplateText))

type ProfileAnalysisPublishRequest struct {
	ExpectedCurrentArtifactID string                         `json:"expected_current_artifact_id"`
	Analysis                  profilemodel.ProfileAnalysisV1 `json:"analysis"`
}

type RendererIdentity struct {
	Version        string `json:"version"`
	TemplateSHA256 string `json:"template_sha256"`
	HTMLSHA256     string `json:"html_sha256"`
}

type ProfileAnalysisCurrent struct {
	SchemaVersion  int              `json:"schema_version"`
	AnalysisID     string           `json:"analysis_id"`
	ArtifactID     string           `json:"artifact_id"`
	CommitSequence uint64           `json:"commit_sequence"`
	SnapshotSHA256 string           `json:"snapshot_sha256"`
	JSONFile       string           `json:"json_file"`
	JSONSHA256     string           `json:"json_sha256"`
	HTMLFile       string           `json:"html_file"`
	HTMLSHA256     string           `json:"html_sha256"`
	CommitFile     string           `json:"commit_file"`
	Renderer       RendererIdentity `json:"renderer"`
}

type ProfileAnalysisCommit struct {
	ProfileAnalysisCurrent
	PreviousCommitSequence uint64 `json:"previous_commit_sequence,omitempty"`
	PreviousCommitFile     string `json:"previous_commit_file,omitempty"`
}

type ProfileAnalysisPublishResponse struct {
	AnalysisID        string            `json:"analysis_id"`
	ArtifactID        string            `json:"artifact_id"`
	CommitSequence    uint64            `json:"commit_sequence"`
	JSONFile          string            `json:"json_file"`
	HTMLFile          string            `json:"html_file"`
	CurrentArtifactID string            `json:"current_artifact_id"`
	Renderer          RendererIdentity  `json:"renderer"`
	Visibility        string            `json:"visibility"`
	Durability        safefs.Durability `json:"durability"`
}

func (h *handler) profileAnalysis(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if h.p.DataDir == "" {
		http.Error(w, "ISUTOOLS_DATA_DIR is not configured", http.StatusBadRequest)
		return
	}
	base := request.URL.Query().Get("snapshot")
	if !validSnapshotBase(base) {
		http.Error(w, "invalid snapshot base", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxAnalysisEnvelopeBytes+1))
	if err != nil {
		http.Error(w, "cannot read analysis request", http.StatusBadRequest)
		return
	}
	if len(body) > maxAnalysisEnvelopeBytes {
		http.Error(w, "analysis request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	var envelope ProfileAnalysisPublishRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		http.Error(w, "invalid analysis request: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(w, "analysis request has trailing JSON", http.StatusUnprocessableEntity)
		return
	}
	if envelope.ExpectedCurrentArtifactID != "none" && !fullLowerHash(envelope.ExpectedCurrentArtifactID) {
		http.Error(w, "expected_current_artifact_id must be none or a full lowercase SHA-256", http.StatusUnprocessableEntity)
		return
	}
	if err := profilemodel.Validate(envelope.Analysis); err != nil {
		http.Error(w, "invalid analysis: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	h.profileAnalysisMu.Lock()
	defer h.profileAnalysisMu.Unlock()
	result, status, err := h.publishProfileAnalysis(base, envelope)
	if err != nil {
		var conflict *profileAnalysisConflict
		if errors.As(err, &conflict) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": conflict.Error(), "current_artifact_id": conflict.currentID, "commit_sequence": conflict.sequence})
			return
		}
		code := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		} else if errors.Is(err, safefs.ErrUnsupportedFilesystem) {
			code = http.StatusServiceUnavailable
		} else if errors.Is(err, errAnalysisMismatch) {
			code = http.StatusUnprocessableEntity
		}
		http.Error(w, "profile analysis publish failed: "+err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}

var errAnalysisMismatch = errors.New("profile analysis does not match snapshot")

type profileAnalysisConflict struct {
	currentID string
	sequence  uint64
	reason    string
}

func (e *profileAnalysisConflict) Error() string { return e.reason }

func (h *handler) publishProfileAnalysis(base string, envelope ProfileAnalysisPublishRequest) (ProfileAnalysisPublishResponse, int, error) {
	root, err := safefs.Open(h.p.DataDir, safefs.Options{RequireStrongVisibility: true, Exclusive: false})
	if err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	defer func() { _ = root.Close() }()
	lock, err := root.TryLock(".isutools-profile-analysis.lock")
	if err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	defer func() { _ = lock.Close() }()

	snapshotBytes, err := root.ReadFile(base+".json", maxSnapshotBytes)
	if err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	originalHTML, err := root.ReadFile(base+".html", maxSnapshotBytes)
	if err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	snapshotSum := sha256.Sum256(snapshotBytes)
	snapshotHash := hex.EncodeToString(snapshotSum[:])
	var payload jsonPayload
	if err := json.Unmarshal(snapshotBytes, &payload); err != nil {
		return ProfileAnalysisPublishResponse{}, 0, fmt.Errorf("%w: decode snapshot: %v", errAnalysisMismatch, err)
	}
	analysis := envelope.Analysis
	if analysis.SnapshotBase != base || analysis.SnapshotSHA256 != snapshotHash ||
		analysis.SnapshotSchemaVersion != payload.Meta.SchemaVersion || payload.Meta.SchemaVersion != schemaVersion {
		return ProfileAnalysisPublishResponse{}, 0, fmt.Errorf("%w: snapshot identity", errAnalysisMismatch)
	}
	runID := ""
	if payload.Meta.Run != nil {
		runID = payload.Meta.Run.RunID
	}
	if runID == "" && payload.Meta.Profiles != nil {
		runID = payload.Meta.Profiles.RunID
	}
	if analysis.RunID != runID || runID == "" {
		return ProfileAnalysisPublishResponse{}, 0, fmt.Errorf("%w: run identity", errAnalysisMismatch)
	}
	if err := validateAnalysisAttempts(root, payload.Meta.Profiles, analysis); err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}

	analysisJSON, err := profilemodel.CanonicalJSON(analysis)
	if err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	var fragment bytes.Buffer
	if err := profileAnalysisTemplate.Execute(&fragment, analysis); err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	if fragment.Len() > maxAnalysisHTMLIncrement {
		return ProfileAnalysisPublishResponse{}, 0, fmt.Errorf("analysis HTML increment exceeds %d bytes", maxAnalysisHTMLIncrement)
	}
	closing := bytes.LastIndex(bytes.ToLower(originalHTML), []byte("</body>"))
	if closing < 0 {
		return ProfileAnalysisPublishResponse{}, 0, fmt.Errorf("%w: original HTML has no body terminator", errAnalysisMismatch)
	}
	derived := make([]byte, 0, len(originalHTML)+fragment.Len())
	derived = append(derived, originalHTML[:closing]...)
	derived = append(derived, fragment.Bytes()...)
	derived = append(derived, originalHTML[closing:]...)
	if len(derived) > maxDerivedHTMLBytes {
		return ProfileAnalysisPublishResponse{}, 0, fmt.Errorf("derived HTML exceeds %d bytes", maxDerivedHTMLBytes)
	}
	jsonHash := hashBytes(analysisJSON)
	htmlHash := hashBytes(derived)
	templateHash := hashBytes([]byte(profileAnalysisTemplateText))
	artifactID := artifactIdentity(snapshotHash, analysis.AnalysisID, profileAnalysisRendererVersion, templateHash, htmlHash)
	renderer := RendererIdentity{Version: profileAnalysisRendererVersion, TemplateSHA256: templateHash, HTMLSHA256: htmlHash}

	currentName := base + ".profile.current.json"
	current, exists, err := readCurrent(root, currentName)
	if err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	if exists && current.ArtifactID == artifactID {
		if err := verifyCurrent(root, current); err != nil {
			return ProfileAnalysisPublishResponse{}, 0, err
		}
		return responseFromCurrent(current, "committed", current.Renderer), http.StatusOK, nil
	}
	currentID := "none"
	if exists {
		currentID = current.ArtifactID
	}
	if envelope.ExpectedCurrentArtifactID != currentID {
		return ProfileAnalysisPublishResponse{}, 0, &profileAnalysisConflict{currentID: currentID, sequence: current.CommitSequence, reason: "expected current artifact does not match"}
	}

	sequence, err := nextCommitSequence(root, base, current.CommitSequence)
	if err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	jsonName := base + ".profile.analysis." + analysis.AnalysisID + ".json"
	htmlName := base + ".profile.render." + artifactID + ".html"
	if err := h.publishImmutable(root, jsonName, analysisJSON); err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	if err := h.publishImmutable(root, htmlName, derived); err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	commitName := fmt.Sprintf("%s.profile.commit.%020d.json", base, sequence)
	next := ProfileAnalysisCurrent{
		SchemaVersion: 1, AnalysisID: analysis.AnalysisID, ArtifactID: artifactID, CommitSequence: sequence,
		SnapshotSHA256: snapshotHash, JSONFile: jsonName, JSONSHA256: jsonHash,
		HTMLFile: htmlName, HTMLSHA256: htmlHash, CommitFile: commitName, Renderer: renderer,
	}
	commit := ProfileAnalysisCommit{ProfileAnalysisCurrent: next}
	if exists {
		commit.PreviousCommitSequence, commit.PreviousCommitFile = current.CommitSequence, current.CommitFile
	}
	commitBytes, err := json.Marshal(commit)
	if err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	if err := h.publishImmutable(root, commitName, commitBytes); err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	markerBytes, err := json.Marshal(next)
	if err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	markerTemp := fmt.Sprintf("%s.profile.current.%06d.tmp", base, h.publishSeq.Add(1))
	if err := writeSyncedTemp(root, markerTemp, markerBytes); err != nil {
		return ProfileAnalysisPublishResponse{}, 0, err
	}
	publication, replaceErr := root.Replace(markerTemp, currentName)
	if replaceErr != nil && !publication.Visible {
		_ = root.Remove(markerTemp)
		return ProfileAnalysisPublishResponse{}, 0, replaceErr
	}
	durability := publication.Durability
	status := http.StatusCreated
	if replaceErr != nil || durability != safefs.DurabilityDurable {
		durability = safefs.DurabilityUnknown
		status = http.StatusAccepted
	}
	response := responseFromCurrent(next, "committed", renderer)
	response.Durability = durability
	if cleanupErr := pruneProfileAnalysisHistory(root, base, next); cleanupErr != nil {
		response.Durability = safefs.DurabilityUnknown
		status = http.StatusAccepted
		if h.p.Health != nil {
			h.p.Health.Set("profile-analysis", health.StatusDegraded, "activation committed; history cleanup failed: "+cleanupErr.Error())
		}
	}
	return response, status, nil
}

func (h *handler) publishImmutable(root *safefs.Root, name string, body []byte) error {
	temp := fmt.Sprintf("%s.%06d.tmp", name, h.publishSeq.Add(1))
	if err := writeSyncedTemp(root, temp, body); err != nil {
		return err
	}
	_, err := root.PublishNoReplace(temp, name)
	if errors.Is(err, safefs.ErrExists) {
		_ = root.Remove(temp)
		existing, readErr := root.ReadFile(name, int64(len(body))+1)
		if readErr == nil && bytes.Equal(existing, body) {
			return nil
		}
		return &profileAnalysisConflict{reason: "immutable artifact already exists with different bytes"}
	}
	if err != nil {
		_ = root.Remove(temp)
	}
	return err
}

func writeSyncedTemp(root *safefs.Root, name string, body []byte) error {
	file, err := root.CreateExclusive(name, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = file.Close()
		_ = root.Remove(name)
		return err
	}
	if _, err := file.Write(body); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(name)
		return err
	}
	return nil
}

func validateAnalysisAttempts(root *safefs.Root, manifest *ProfileManifest, analysis profilemodel.ProfileAnalysisV1) error {
	if manifest == nil {
		return fmt.Errorf("%w: profile manifest is missing", errAnalysisMismatch)
	}
	if manifest.Executable == nil || !reflect.DeepEqual(analysis.Binary.Captured, profileModelExecutable(*manifest.Executable)) {
		return fmt.Errorf("%w: captured executable identity", errAnalysisMismatch)
	}
	type expectedGroup struct {
		mode   string
		inputs map[string]string
		hashes map[string]string
	}
	expected := make(map[string]expectedGroup)
	for _, expectation := range manifest.Expected {
		if _, duplicate := expected[expectation.Kind]; duplicate {
			return fmt.Errorf("%w: duplicate manifest expectation %s", errAnalysisMismatch, expectation.Kind)
		}
		group := expectedGroup{mode: expectation.Mode, inputs: make(map[string]string), hashes: make(map[string]string)}
		for _, input := range expectation.Inputs {
			if input.Kind != expectation.Kind || input.File == "" {
				return fmt.Errorf("%w: malformed manifest expectation %s", errAnalysisMismatch, expectation.Kind)
			}
			group.inputs[input.File] = input.Point
		}
		expected[expectation.Kind] = group
	}
	if len(expected) == 0 {
		// Backward-compatible fallback for snapshots written before explicit
		// expected inputs were added.
		if manifest.CPU != nil && manifest.CPU.ExpectedFile != "" {
			expected["cpu"] = expectedGroup{mode: profilemodel.ProfileModeInterval, inputs: map[string]string{manifest.CPU.ExpectedFile: "interval"}, hashes: make(map[string]string)}
		}
		for _, pair := range manifest.Pairs {
			expected[pair.Kind] = expectedGroup{mode: profilemodel.ProfileModeCumulativeDelta, inputs: map[string]string{pair.OpenFile: "open", pair.CloseFile: "close"}, hashes: make(map[string]string)}
		}
	}
	for _, capture := range manifest.Captures {
		if capture.File != "" && capture.SHA256 != "" {
			group := expected[capture.Kind]
			group.hashes[capture.File] = capture.SHA256
			expected[capture.Kind] = group
		}
	}
	if manifest.CPU != nil && manifest.CPU.File != "" && manifest.CPU.SHA256 != "" {
		group := expected["cpu"]
		group.hashes[manifest.CPU.File] = manifest.CPU.SHA256
		expected["cpu"] = group
	}
	if len(expected) != len(analysis.Attempts) {
		return fmt.Errorf("%w: attempt count", errAnalysisMismatch)
	}
	seen := make(map[string]struct{}, len(analysis.Attempts))
	for _, attempt := range analysis.Attempts {
		group, ok := expected[attempt.Kind]
		if !ok || group.mode != attempt.Mode {
			return fmt.Errorf("%w: unexpected attempt %s", errAnalysisMismatch, attempt.Kind)
		}
		if _, duplicate := seen[attempt.Kind]; duplicate {
			return fmt.Errorf("%w: duplicate attempt %s", errAnalysisMismatch, attempt.Kind)
		}
		seen[attempt.Kind] = struct{}{}
		if len(group.inputs) != len(attempt.ExpectedInputs) {
			return fmt.Errorf("%w: expected input count for %s", errAnalysisMismatch, attempt.Kind)
		}
		for _, input := range attempt.ExpectedInputs {
			if point, ok := group.inputs[input.File]; !ok || point != input.Point || input.Kind != attempt.Kind {
				return fmt.Errorf("%w: expected input %s", errAnalysisMismatch, input.File)
			}
		}
		for _, observed := range attempt.ObservedInputs {
			if _, ok := group.inputs[observed.File]; !ok {
				return fmt.Errorf("%w: observed input %s", errAnalysisMismatch, observed.File)
			}
			body, err := root.ReadFile(observed.File, 32<<20)
			missingPinned := errors.Is(err, os.ErrNotExist) && group.hashes[observed.File] == observed.SHA256
			if err != nil && !missingPinned {
				return fmt.Errorf("%w: observed input %s: %v", errAnalysisMismatch, observed.File, err)
			}
			if !missingPinned && (int64(len(body)) != observed.Bytes || hashBytes(body) != observed.SHA256) {
				return fmt.Errorf("%w: observed input hash %s", errAnalysisMismatch, observed.File)
			}
			if capturedHash := group.hashes[observed.File]; capturedHash != "" && capturedHash != observed.SHA256 {
				return fmt.Errorf("%w: manifest hash %s", errAnalysisMismatch, observed.File)
			}
			if attempt.Kind != "cpu" {
				if observed.Sidecar != "" || observed.SidecarSHA256 != "" || observed.CoverageFile != "" || observed.CoverageSHA256 != "" {
					return fmt.Errorf("%w: unexpected completion evidence for %s", errAnalysisMismatch, attempt.Kind)
				}
				continue
			}
			if manifest.CPU == nil {
				return fmt.Errorf("%w: CPU manifest is missing", errAnalysisMismatch)
			}
			if err := validateAnalysisAttachment(root, "sidecar", observed.Sidecar, observed.SidecarSHA256, manifest.CPU.Sidecar, manifest.CPU.SidecarSHA256); err != nil {
				return err
			}
			if err := validateAnalysisAttachment(root, "coverage", observed.CoverageFile, observed.CoverageSHA256, manifest.CPU.CoverageFile, manifest.CPU.CoverageSHA256); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateAnalysisAttachment(root *safefs.Root, kind, file, hash, capturedFile, capturedHash string) error {
	if file != capturedFile || hash != capturedHash || (file == "") != (hash == "") {
		return fmt.Errorf("%w: %s identity", errAnalysisMismatch, kind)
	}
	if file == "" {
		return nil
	}
	body, err := root.ReadFile(file, int64(profileSidecarMaxBytes))
	if errors.Is(err, os.ErrNotExist) && hash == capturedHash {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", errAnalysisMismatch, kind, file, err)
	}
	if hashBytes(body) != hash {
		return fmt.Errorf("%w: %s hash %s", errAnalysisMismatch, kind, file)
	}
	return nil
}

func profileModelExecutable(identity buildinfo.ExecutableIdentity) profilemodel.ExecutableIdentity {
	var settings []profilemodel.BuildSetting
	if len(identity.Settings) != 0 {
		settings = make([]profilemodel.BuildSetting, len(identity.Settings))
		for i, setting := range identity.Settings {
			settings[i] = profilemodel.BuildSetting{Key: setting.Key, Value: setting.Value}
		}
	}
	return profilemodel.ExecutableIdentity{
		SHA256: identity.SHA256, BuildInfoSHA256: identity.BuildInfoSHA256, Source: identity.Source,
		GoVersion: identity.GoVersion, MainModule: identity.MainModule, MainVersion: identity.MainVersion,
		MainSum: identity.MainSum, VCSRevision: identity.VCSRevision, VCSModified: identity.VCSModified,
		Settings: settings, Status: identity.Status,
	}
}

func readCurrent(root *safefs.Root, name string) (ProfileAnalysisCurrent, bool, error) {
	body, err := root.ReadFile(name, 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return ProfileAnalysisCurrent{}, false, nil
	}
	if err != nil {
		return ProfileAnalysisCurrent{}, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var current ProfileAnalysisCurrent
	if err := decoder.Decode(&current); err != nil {
		return ProfileAnalysisCurrent{}, false, fmt.Errorf("invalid current marker: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ProfileAnalysisCurrent{}, false, errors.New("invalid current marker trailing data")
	}
	if current.SchemaVersion != 1 || !fullLowerHash(current.AnalysisID) || !fullLowerHash(current.ArtifactID) ||
		!fullLowerHash(current.SnapshotSHA256) || !fullLowerHash(current.JSONSHA256) || !fullLowerHash(current.HTMLSHA256) || current.CommitSequence == 0 {
		return ProfileAnalysisCurrent{}, false, errors.New("invalid current marker identity")
	}
	base, ok := strings.CutSuffix(name, ".profile.current.json")
	if !ok || current.JSONFile != base+".profile.analysis."+current.AnalysisID+".json" ||
		current.HTMLFile != base+".profile.render."+current.ArtifactID+".html" ||
		current.CommitFile != fmt.Sprintf("%s.profile.commit.%020d.json", base, current.CommitSequence) ||
		current.Renderer.Version == "" || current.Renderer.TemplateSHA256 != hashBytes([]byte(profileAnalysisTemplateText)) ||
		current.Renderer.HTMLSHA256 != current.HTMLSHA256 {
		return ProfileAnalysisCurrent{}, false, errors.New("invalid current marker filenames or renderer")
	}
	return current, true, nil
}

// serveCurrentDerived follows only a strict, hash-verified current marker.
// Corrupt or incomplete derived state fails open to the immutable original.
func (h *handler) serveCurrentDerived(w http.ResponseWriter, request *http.Request, base string) bool {
	root, err := h.openDataRoot()
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	current, exists, err := readCurrent(root, base+".profile.current.json")
	if err != nil || !exists {
		return false
	}
	snapshot, err := root.ReadFile(base+".json", maxSnapshotBytes)
	if err != nil || hashBytes(snapshot) != current.SnapshotSHA256 || verifyCurrent(root, current) != nil {
		return false
	}
	h.serveDataFile(w, request, current.HTMLFile)
	return true
}

func verifyCurrent(root *safefs.Root, current ProfileAnalysisCurrent) error {
	if err := verifyActivationCommit(root, current); err != nil {
		return err
	}
	jsonBytes, err := root.ReadFile(current.JSONFile, profilemodel.MaxAnalysisBodyBytes)
	if err != nil || hashBytes(jsonBytes) != current.JSONSHA256 {
		return errors.New("current analysis JSON hash mismatch")
	}
	htmlBytes, err := root.ReadFile(current.HTMLFile, maxDerivedHTMLBytes)
	if err != nil || hashBytes(htmlBytes) != current.HTMLSHA256 {
		return errors.New("current derived HTML hash mismatch")
	}
	return nil
}

func verifyActivationCommit(root *safefs.Root, current ProfileAnalysisCurrent) error {
	body, err := root.ReadFile(current.CommitFile, 64<<10)
	if err != nil {
		return fmt.Errorf("current activation commit is unavailable: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var commit ProfileAnalysisCommit
	if err := decoder.Decode(&commit); err != nil {
		return fmt.Errorf("current activation commit is invalid: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("current activation commit has trailing data")
	}
	if !reflect.DeepEqual(commit.ProfileAnalysisCurrent, current) {
		return errors.New("current activation commit does not match marker")
	}
	if (commit.PreviousCommitSequence == 0) != (commit.PreviousCommitFile == "") ||
		commit.PreviousCommitSequence >= commit.CommitSequence {
		return errors.New("current activation commit has an invalid predecessor")
	}
	if commit.PreviousCommitFile != "" {
		base := strings.TrimSuffix(current.CommitFile, fmt.Sprintf(".profile.commit.%020d.json", current.CommitSequence))
		if commit.PreviousCommitFile != fmt.Sprintf("%s.profile.commit.%020d.json", base, commit.PreviousCommitSequence) {
			return errors.New("current activation commit predecessor filename is invalid")
		}
	}
	return nil
}

type analysisCommitEntry struct {
	name     string
	sequence uint64
}

func pruneProfileAnalysisHistory(root *safefs.Root, base string, current ProfileAnalysisCurrent) error {
	entries, err := root.ReadDir()
	if err != nil {
		return err
	}
	commits := make([]analysisCommitEntry, 0, profileAnalysisHistory)
	for _, entry := range entries {
		sequence, ok, err := parseAnalysisCommitName(base, entry.Name())
		if err != nil {
			return err
		}
		if ok {
			commits = append(commits, analysisCommitEntry{name: entry.Name(), sequence: sequence})
		}
	}
	sort.Slice(commits, func(i, j int) bool { return commits[i].sequence > commits[j].sequence })
	if len(commits) > maxCommitFiles {
		return errors.New("profile analysis commit scan exceeds limit")
	}

	keep := map[string]struct{}{
		current.CommitFile: {}, current.JSONFile: {}, current.HTMLFile: {},
	}
	for i, entry := range commits {
		if i >= profileAnalysisHistory {
			continue
		}
		body, err := root.ReadFile(entry.name, 64<<10)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var commit ProfileAnalysisCommit
		if err := decoder.Decode(&commit); err != nil {
			return fmt.Errorf("decode retained activation %s: %w", entry.name, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return fmt.Errorf("retained activation %s has trailing data", entry.name)
		}
		if commit.CommitFile != entry.name || commit.CommitSequence != entry.sequence ||
			!strictAnalysisJSONName(base, commit.JSONFile) || !strictAnalysisHTMLName(base, commit.HTMLFile) {
			return fmt.Errorf("retained activation %s is inconsistent", entry.name)
		}
		keep[entry.name], keep[commit.JSONFile], keep[commit.HTMLFile] = struct{}{}, struct{}{}, struct{}{}
	}

	for _, entry := range entries {
		name := entry.Name()
		_, retained := keep[name]
		_, commitName, commitErr := parseAnalysisCommitName(base, name)
		if commitErr != nil {
			return commitErr
		}
		managed := commitName || strictAnalysisJSONName(base, name) || strictAnalysisHTMLName(base, name)
		if managed && !retained {
			if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func strictAnalysisJSONName(base, name string) bool {
	id, ok := strings.CutSuffix(strings.TrimPrefix(name, base+".profile.analysis."), ".json")
	return strings.HasPrefix(name, base+".profile.analysis.") && ok && fullLowerHash(id)
}

func strictAnalysisHTMLName(base, name string) bool {
	id, ok := strings.CutSuffix(strings.TrimPrefix(name, base+".profile.render."), ".html")
	return strings.HasPrefix(name, base+".profile.render.") && ok && fullLowerHash(id)
}

func nextCommitSequence(root *safefs.Root, base string, current uint64) (uint64, error) {
	entries, err := root.ReadDir()
	if err != nil {
		return 0, err
	}
	maxSequence, count := current, 0
	for _, entry := range entries {
		sequence, ok, err := parseAnalysisCommitName(base, entry.Name())
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		count++
		if count > maxCommitFiles {
			return 0, errors.New("profile analysis commit scan exceeds limit")
		}
		if sequence > maxSequence {
			maxSequence = sequence
		}
	}
	if maxSequence == math.MaxUint64 {
		return 0, errors.New("profile analysis commit sequence overflow")
	}
	return maxSequence + 1, nil
}

func parseAnalysisCommitName(base, name string) (uint64, bool, error) {
	prefix := base + ".profile.commit."
	if !strings.HasPrefix(name, prefix) {
		return 0, false, nil
	}
	digits, ok := strings.CutSuffix(strings.TrimPrefix(name, prefix), ".json")
	if !ok || len(digits) != 20 {
		return 0, false, errors.New("invalid profile analysis commit filename")
	}
	sequence, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || sequence == 0 {
		return 0, false, errors.New("invalid profile analysis commit sequence")
	}
	return sequence, true, nil
}

func responseFromCurrent(current ProfileAnalysisCurrent, visibility string, renderer RendererIdentity) ProfileAnalysisPublishResponse {
	return ProfileAnalysisPublishResponse{
		AnalysisID: current.AnalysisID, ArtifactID: current.ArtifactID, CommitSequence: current.CommitSequence,
		JSONFile: current.JSONFile, HTMLFile: current.HTMLFile, CurrentArtifactID: current.ArtifactID,
		Renderer: renderer, Visibility: visibility, Durability: safefs.DurabilityUnknown,
	}
}

func artifactIdentity(fields ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("isutools-profile-artifact-v1\x00"))
	for _, field := range fields {
		_, _ = hash.Write([]byte(field))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func hashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func fullLowerHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for i := range value {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}

func validSnapshotBase(base string) bool {
	if base == "" || len(base) > 255 || strings.ContainsAny(base, "/\\") {
		return false
	}
	first, _, ok := strings.Cut(base, "_")
	return ok && runIDPattern.MatchString(first)
}
