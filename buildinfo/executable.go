package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
)

const (
	SourceProcSelfExe = "proc-self-exe"
	SourceInputFile   = "input-file"
	SourcePathUnbound = "path-unbound"
	SourceUnavailable = "unavailable"

	ExecutableStatusCaptured          = "captured"
	ExecutableStatusUnavailable       = "unavailable"
	ExecutableStatusChangedDuringRead = "changed-during-read"

	maxExecutableBytes = int64(1 << 30)
)

type BuildSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type ExecutableIdentity struct {
	SHA256          string         `json:"sha256,omitempty"`
	BuildInfoSHA256 string         `json:"build_info_sha256,omitempty"`
	Source          string         `json:"source"`
	GoVersion       string         `json:"go_version,omitempty"`
	MainModule      string         `json:"main_module,omitempty"`
	MainVersion     string         `json:"main_version,omitempty"`
	MainSum         string         `json:"main_sum,omitempty"`
	VCSRevision     string         `json:"vcs_revision,omitempty"`
	VCSModified     bool           `json:"vcs_modified,omitempty"`
	Settings        []BuildSetting `json:"settings,omitempty"`
	Status          string         `json:"status"`
}

func (identity ExecutableIdentity) String() string {
	parts := []string{"source=" + identity.Source, "status=" + identity.Status}
	for _, setting := range identity.Settings {
		parts = append(parts, setting.Key+"="+setting.Value)
	}
	return strings.Join(parts, " ")
}

// CaptureExecutable hashes an open descriptor for the running image on Linux.
// Other platforms deliberately record path-unbound provenance: matching that
// hash is useful evidence, but can never be promoted to verified identity.
func CaptureExecutable() (ExecutableIdentity, error) {
	path := ""
	source := SourcePathUnbound
	if runtime.GOOS == "linux" {
		path, source = "/proc/self/exe", SourceProcSelfExe
	} else {
		var err error
		path, err = os.Executable()
		if err != nil {
			return ExecutableIdentity{Source: SourceUnavailable, Status: ExecutableStatusUnavailable}, err
		}
	}
	return captureFile(path, source, debug.ReadBuildInfo, nil)
}

// CaptureInputFile hashes a user-selected analysis binary without executing
// it. The path is never retained in the returned provenance record.
func CaptureInputFile(path string) (ExecutableIdentity, error) {
	return captureFile(path, SourceInputFile, nil, nil)
}

func captureFile(path, source string, readBuildInfo func() (*debug.BuildInfo, bool), afterRead func()) (ExecutableIdentity, error) {
	identity := ExecutableIdentity{Source: source, Status: ExecutableStatusUnavailable}
	file, err := os.Open(path)
	if err != nil {
		return identity, fmt.Errorf("buildinfo: open executable image: %w", err)
	}
	defer func() { _ = file.Close() }()
	before, err := file.Stat()
	if err != nil {
		return identity, fmt.Errorf("buildinfo: stat executable image: %w", err)
	}
	if !before.Mode().IsRegular() {
		return identity, errors.New("buildinfo: executable image is not regular")
	}
	if before.Size() < 0 || before.Size() > maxExecutableBytes {
		return identity, fmt.Errorf("buildinfo: executable image exceeds %d bytes", maxExecutableBytes)
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(file, maxExecutableBytes+1))
	if err != nil {
		return identity, fmt.Errorf("buildinfo: hash executable image: %w", err)
	}
	if written > maxExecutableBytes {
		return identity, fmt.Errorf("buildinfo: executable image exceeds %d bytes", maxExecutableBytes)
	}
	if afterRead != nil {
		afterRead()
	}
	afterFD, fdErr := file.Stat()
	afterPath, pathErr := os.Stat(path)
	if fdErr != nil || pathErr != nil || !os.SameFile(before, afterFD) || !os.SameFile(before, afterPath) || before.Size() != afterFD.Size() {
		identity.Status = ExecutableStatusChangedDuringRead
		return identity, errors.New("buildinfo: executable identity changed during capture")
	}
	identity.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	identity.Status = ExecutableStatusCaptured
	identity.applyBuildInfo(readBuildInfo)
	return identity, nil
}

func (identity *ExecutableIdentity) applyBuildInfo(read func() (*debug.BuildInfo, bool)) {
	if read == nil {
		return
	}
	build, ok := read()
	if !ok || build == nil {
		return
	}
	identity.GoVersion = bounded(build.GoVersion, 256)
	identity.MainModule = bounded(build.Main.Path, 256)
	identity.MainVersion = bounded(build.Main.Version, 256)
	identity.MainSum = bounded(build.Main.Sum, 256)
	settings := map[string]string{"GOOS": runtime.GOOS, "GOARCH": runtime.GOARCH}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			identity.VCSRevision = bounded(setting.Value, 256)
		case "vcs.modified":
			identity.VCSModified = setting.Value == "true"
		case "GOOS", "GOARCH", "CGO_ENABLED", "-buildmode", "-compiler", "-tags", "-trimpath",
			"GO386", "GOAMD64", "GOARM", "GOARM64", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM":
			settings[setting.Key] = bounded(setting.Value, 256)
		case "-pgo":
			settings["pgo"] = presence(setting.Value)
		case "-ldflags":
			settings["ldflags"] = presence(setting.Value)
		}
	}
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	identity.Settings = make([]BuildSetting, 0, len(keys))
	for _, key := range keys {
		if settings[key] != "" {
			identity.Settings = append(identity.Settings, BuildSetting{Key: key, Value: settings[key]})
		}
	}
	projection := struct {
		GoVersion   string         `json:"go_version"`
		MainModule  string         `json:"main_module"`
		MainVersion string         `json:"main_version"`
		MainSum     string         `json:"main_sum"`
		VCSRevision string         `json:"vcs_revision"`
		VCSModified bool           `json:"vcs_modified"`
		Settings    []BuildSetting `json:"settings"`
	}{identity.GoVersion, identity.MainModule, identity.MainVersion, identity.MainSum, identity.VCSRevision, identity.VCSModified, identity.Settings}
	body, _ := json.Marshal(projection)
	sum := sha256.Sum256(body)
	identity.BuildInfoSHA256 = hex.EncodeToString(sum[:])
}

func presence(value string) string {
	if value == "" {
		return "absent"
	}
	return "present"
}

func bounded(value string, max int) string {
	if len(value) > max {
		return value[:max]
	}
	return value
}
