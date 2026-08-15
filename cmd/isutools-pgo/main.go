package main

import (
	"bytes"
	"crypto/sha256"
	debugbuildinfo "debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	localbuildinfo "github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/internal/pgoworkflow"
	"github.com/ekusiadadus/isutools/internal/safefs"
	"github.com/ekusiadadus/isutools/web"
	"golang.org/x/sys/unix"
)

const maxSnapshotBytes = int64(32 << 20)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" || (len(args) == 2 && (args[1] == "-h" || args[1] == "--help")) {
		usage(stdout)
		return 0
	}
	var err error
	switch args[0] {
	case "prepare":
		err = runPrepare(args[1:], stdout)
	case "build":
		err = runBuild(args[1:], stdout)
	case "verify-build":
		err = runVerifyBuild(args[1:], stdout)
	default:
		usage(stderr)
		return 2
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		_, _ = fmt.Fprintln(stderr, "isutools-pgo: invalid-flags")
		return 2
	}
	_, _ = fmt.Fprintln(stderr, err)
	return 1
}

func usage(output io.Writer) {
	_, _ = fmt.Fprintln(output, "usage: isutools-pgo prepare --snapshot FILE --snapshot-sha256 SHA --profile FILE --binary FILE --source-dir DIR --main-package PKG --output-dir NEW_DIR --rationale TEXT")
	_, _ = fmt.Fprintln(output, "       isutools-pgo build --candidate-dir DIR --source-dir DIR --variant pgo|off")
	_, _ = fmt.Fprintln(output, "       isutools-pgo verify-build --candidate-dir DIR --binary FILE --variant pgo|off")
}

func runPrepare(args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("prepare", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	snapshotPath := set.String("snapshot", "", "saved snapshot JSON")
	snapshotSHA := set.String("snapshot-sha256", "", "ledger snapshot SHA-256")
	profilePath := set.String("profile", "", "saved CPU profile")
	binaryPath := set.String("binary", "", "matching capture-time binary")
	sourceDir := set.String("source-dir", "", "clean source checkout")
	mainPackage := set.String("main-package", "", "Go main package")
	outputDir := set.String("output-dir", "", "new private candidate directory")
	rationale := set.String("rationale", "", "bounded representative workload rationale")
	if err := set.Parse(args); err != nil || set.NArg() != 0 {
		return flag.ErrHelp
	}
	if *snapshotPath == "" || *snapshotSHA == "" || *profilePath == "" || *binaryPath == "" || *sourceDir == "" || *mainPackage == "" || *outputDir == "" || *rationale == "" {
		return flag.ErrHelp
	}
	snapshotBody, err := readRegular(*snapshotPath, maxSnapshotBytes)
	if err != nil || hashBytes(snapshotBody) != *snapshotSHA {
		return errors.New("isutools-pgo: snapshot-sha-mismatch")
	}
	var persisted struct {
		web.Snapshot
		Prev json.RawMessage `json:"prev,omitempty"`
	}
	if err := json.Unmarshal(snapshotBody, &persisted); err != nil || persisted.Meta.Run == nil || persisted.Meta.Profiles == nil || persisted.Meta.Profiles.CPU == nil || persisted.Meta.Profiles.Executable == nil {
		return errors.New("isutools-pgo: snapshot-has-no-cpu-profile-provenance")
	}
	cpu := persisted.Meta.Profiles.CPU
	if !cpu.Complete || cpu.Status != "published" || cpu.File == "" || filepath.Base(*profilePath) != cpu.File {
		return errors.New("isutools-pgo: cpu-profile-not-complete")
	}
	profileBody, err := readRegular(*profilePath, pgoworkflow.MaxProfileBytes)
	if err != nil || int64(len(profileBody)) != cpu.Bytes || hashBytes(profileBody) != cpu.SHA256 {
		return errors.New("isutools-pgo: profile-sha-mismatch")
	}
	if err := pgoworkflow.ValidateCPUProfile(profileBody); err != nil {
		return err
	}
	binary, err := localbuildinfo.CaptureInputFile(*binaryPath)
	if err != nil || binary.SHA256 != persisted.Meta.Profiles.Executable.SHA256 {
		return errors.New("isutools-pgo: binary-sha-mismatch")
	}
	revision, dirty, err := sourceState(*sourceDir)
	if err != nil || dirty {
		return errors.New("isutools-pgo: source-must-be-a-clean-git-checkout")
	}
	toolchain, err := goVersion()
	if err != nil {
		return errors.New("isutools-pgo: go-toolchain-unavailable")
	}
	if persisted.Meta.Profiles.Executable.GoVersion == "" || persisted.Meta.Profiles.Executable.GoVersion != toolchain ||
		persisted.Meta.Profiles.Executable.VCSRevision == "" || persisted.Meta.Profiles.Executable.VCSRevision != revision || persisted.Meta.Profiles.Executable.VCSModified ||
		persisted.Meta.Profiles.Executable.Status != localbuildinfo.ExecutableStatusCaptured || persisted.Meta.Profiles.Executable.Source != localbuildinfo.SourceProcSelfExe {
		return errors.New("isutools-pgo: source-or-toolchain-provenance-mismatch")
	}
	if err := validateMainPackage(*sourceDir, *mainPackage); err != nil {
		return errors.New("isutools-pgo: main-package-is-not-buildable-main")
	}
	candidate, err := pgoworkflow.Prepare(pgoworkflow.Input{
		RunID: persisted.Meta.Run.RunID, SnapshotBase: strings.TrimSuffix(filepath.Base(*snapshotPath), filepath.Ext(*snapshotPath)), SnapshotSHA256: *snapshotSHA,
		ProfileSHA256: cpu.SHA256, ProfileBytes: uint64(cpu.Bytes), CapturedBinarySHA256: binary.SHA256,
		SourceRevision: revision, SourceDirty: dirty, Toolchain: toolchain, MainPackage: *mainPackage, Rationale: *rationale, CreatedAt: time.Now(),
	})
	if err != nil {
		return err
	}
	manifest, err := pgoworkflow.Encode(candidate)
	if err != nil {
		return err
	}
	if err := os.Mkdir(*outputDir, 0o700); err != nil {
		return errors.New("isutools-pgo: output-dir-must-not-exist")
	}
	root, err := safefs.Open(*outputDir, safefs.Options{RequireStrongVisibility: true, Exclusive: true})
	if err != nil {
		return errors.New("isutools-pgo: unsafe-output-filesystem")
	}
	defer func() { _ = root.Close() }()
	if err := publish(root, "default.pgo", profileBody); err != nil {
		return errors.New("isutools-pgo: profile-publication-failed")
	}
	if err := publish(root, "candidate.json", manifest); err != nil {
		return errors.New("isutools-pgo: manifest-publication-failed")
	}
	readyBody, _ := json.Marshal(map[string]string{"candidate_id": candidate.CandidateID, "manifest_sha256": hashBytes(manifest), "profile_sha256": cpu.SHA256})
	if err := publish(root, "candidate.ready.json", append(readyBody, '\n')); err != nil {
		return errors.New("isutools-pgo: ready-marker-publication-failed")
	}
	if _, err := fmt.Fprintf(stdout, "candidate=%s\nbuild: go build -pgo=%s/default.pgo -o OUTPUT_BINARY %s\nrollback: go build -pgo=off -o OUTPUT_BINARY %s\n", candidate.CandidateID, shellQuote(*outputDir), shellQuote(*mainPackage), shellQuote(*mainPackage)); err != nil {
		return errors.New("isutools-pgo: output-write-failed")
	}
	return nil
}

type buildVerification struct {
	Schema         string `json:"schema"`
	CandidateID    string `json:"candidate_id"`
	Variant        string `json:"variant"`
	Binary         string `json:"binary,omitempty"`
	BinarySHA256   string `json:"binary_sha256"`
	BinaryBytes    int64  `json:"binary_bytes"`
	GoVersion      string `json:"go_version"`
	PGOEnabled     bool   `json:"pgo_enabled"`
	SourceRevision string `json:"source_revision,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	DurationNS     int64  `json:"duration_ns,omitempty"`
	VerifiedAt     string `json:"verified_at"`
}

func runBuild(args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("build", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	directory := set.String("candidate-dir", "", "private candidate directory")
	sourceDir := set.String("source-dir", "", "clean source checkout")
	variant := set.String("variant", "", "pgo or off")
	if err := set.Parse(args); err != nil || set.NArg() != 0 || *directory == "" || *sourceDir == "" || (*variant != "pgo" && *variant != "off") {
		return flag.ErrHelp
	}
	absoluteCandidate, err := filepath.Abs(*directory)
	if err != nil {
		return errors.New("isutools-pgo: candidate-unavailable")
	}
	root, err := safefs.Open(absoluteCandidate, safefs.Options{RequireStrongVisibility: true, Exclusive: true})
	if err != nil {
		return errors.New("isutools-pgo: candidate-unavailable")
	}
	defer func() { _ = root.Close() }()
	manifestBody, err := root.ReadFile("candidate.json", pgoworkflow.MaxManifestBytes)
	if err != nil {
		return errors.New("isutools-pgo: candidate-unavailable")
	}
	candidate, err := pgoworkflow.Decode(bytes.NewReader(manifestBody))
	if err != nil {
		return err
	}
	if time.Now().After(candidate.ExpiresAt) {
		return errors.New("isutools-pgo: candidate-expired")
	}
	profileBody, err := root.ReadFile("default.pgo", pgoworkflow.MaxProfileBytes)
	if err != nil || uint64(len(profileBody)) != candidate.Profile.Bytes || hashBytes(profileBody) != candidate.Profile.SHA256 || pgoworkflow.ValidateCPUProfile(profileBody) != nil {
		return errors.New("isutools-pgo: candidate-profile-mismatch")
	}
	revision, dirty, err := sourceState(*sourceDir)
	if err != nil || dirty || revision != candidate.SourceRevision {
		return errors.New("isutools-pgo: source-must-match-clean-candidate-revision")
	}
	toolchain, err := goVersion()
	if err != nil || toolchain != candidate.Toolchain || validateMainPackage(*sourceDir, candidate.MainPackage) != nil {
		return errors.New("isutools-pgo: build-input-provenance-mismatch")
	}
	binaryName := "candidate-" + *variant + ".bin"
	metadataName := "build-" + *variant + ".json"
	tempName := "." + binaryName + ".building"
	for _, name := range []string{binaryName, metadataName, tempName} {
		if _, statErr := os.Lstat(filepath.Join(absoluteCandidate, name)); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
			return errors.New("isutools-pgo: build-output-must-not-exist")
		}
	}
	profilePath := filepath.Join(absoluteCandidate, "default.pgo")
	tempPath := filepath.Join(absoluteCandidate, tempName)
	pgoFlag := "-pgo=off"
	if *variant == "pgo" {
		pgoFlag = "-pgo=" + profilePath
	}
	started := time.Now().UTC()
	command := exec.Command("go", "build", pgoFlag, "-o", tempPath, candidate.MainPackage)
	command.Dir = *sourceDir
	command.Env = controlledGoEnvironment()
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		_ = root.Remove(tempName)
		return errors.New("isutools-pgo: go-build-failed")
	}
	finished := time.Now().UTC()
	fail := func(cause error) error {
		_ = root.Remove(tempName)
		return cause
	}
	revisionAfter, dirtyAfter, stateErr := sourceState(*sourceDir)
	if stateErr != nil || dirtyAfter || revisionAfter != revision {
		return fail(errors.New("isutools-pgo: source-changed-during-build"))
	}
	if err := os.Chmod(tempPath, 0o700); err != nil {
		return fail(errors.New("isutools-pgo: build-output-permission-failed"))
	}
	tempFile, err := root.OpenRegular(tempName)
	if err != nil {
		return fail(errors.New("isutools-pgo: build-output-invalid"))
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fail(errors.New("isutools-pgo: build-output-sync-failed"))
	}
	_ = tempFile.Close()
	publication, err := root.PublishNoReplace(tempName, binaryName)
	if err != nil && !publication.Visible {
		return fail(errors.New("isutools-pgo: build-output-publication-failed"))
	}
	verification, err := verifyBuiltBinary(candidate, filepath.Join(absoluteCandidate, binaryName), *variant)
	if err != nil {
		return errors.New("isutools-pgo: build-provenance-mismatch")
	}
	verification.Binary = binaryName
	verification.SourceRevision = revision
	verification.StartedAt = started.Format(time.RFC3339Nano)
	verification.FinishedAt = finished.Format(time.RFC3339Nano)
	verification.DurationNS = finished.Sub(started).Nanoseconds()
	verification.VerifiedAt = time.Now().UTC().Format(time.RFC3339Nano)
	metadata, err := json.MarshalIndent(verification, "", "  ")
	if err != nil || publish(root, metadataName, append(metadata, '\n')) != nil {
		return errors.New("isutools-pgo: build-manifest-publication-failed")
	}
	return json.NewEncoder(stdout).Encode(verification)
}

func runVerifyBuild(args []string, stdout io.Writer) error {
	set := flag.NewFlagSet("verify-build", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	directory := set.String("candidate-dir", "", "candidate directory")
	binaryPath := set.String("binary", "", "built binary")
	variant := set.String("variant", "", "pgo or off")
	if err := set.Parse(args); err != nil || set.NArg() != 0 || *directory == "" || *binaryPath == "" || (*variant != "pgo" && *variant != "off") {
		return flag.ErrHelp
	}
	root, err := safefs.Open(*directory, safefs.Options{})
	if err != nil {
		return errors.New("isutools-pgo: candidate-unavailable")
	}
	defer func() { _ = root.Close() }()
	body, err := root.ReadFile("candidate.json", pgoworkflow.MaxManifestBytes)
	if err != nil {
		return errors.New("isutools-pgo: candidate-unavailable")
	}
	candidate, err := pgoworkflow.Decode(bytes.NewReader(body))
	if err != nil {
		return err
	}
	verification, err := verifyBuiltBinary(candidate, *binaryPath, *variant)
	if err != nil {
		return err
	}
	verification.VerifiedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return json.NewEncoder(stdout).Encode(verification)
}

func verifyBuiltBinary(candidate pgoworkflow.Candidate, binaryPath, variant string) (buildVerification, error) {
	info, err := debugbuildinfo.ReadFile(binaryPath)
	if err != nil {
		return buildVerification{}, errors.New("isutools-pgo: binary-build-info-unavailable")
	}
	pgo := false
	for _, setting := range info.Settings {
		if setting.Key == "-pgo" && setting.Value != "" && setting.Value != "off" {
			pgo = true
		}
	}
	if (variant == "pgo") != pgo || info.GoVersion != candidate.Toolchain {
		return buildVerification{}, errors.New("isutools-pgo: build-provenance-mismatch")
	}
	binaryBody, err := readRegular(binaryPath, 1<<30)
	if err != nil {
		return buildVerification{}, errors.New("isutools-pgo: binary-unavailable")
	}
	return buildVerification{Schema: "isutools.pgo-build-verification/v1", CandidateID: candidate.CandidateID,
		Variant: variant, BinarySHA256: hashBytes(binaryBody), BinaryBytes: int64(len(binaryBody)), GoVersion: info.GoVersion, PGOEnabled: pgo}, nil
}

func sourceState(directory string) (string, bool, error) {
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return "", false, errors.New("invalid source directory")
	}
	revisionOutput, err := exec.Command("git", "-C", directory, "rev-parse", "--verify", "HEAD").Output()
	if err != nil {
		return "", false, err
	}
	revision := strings.TrimSpace(string(revisionOutput))
	dirtyCommand := exec.Command("git", "-C", directory, "status", "--porcelain", "--untracked-files=normal")
	dirtyOutput, err := dirtyCommand.Output()
	if err != nil {
		return "", false, err
	}
	return revision, len(dirtyOutput) != 0, nil
}

func goVersion() (string, error) {
	command := exec.Command("go", "env", "GOVERSION")
	command.Env = controlledGoEnvironment()
	body, err := command.Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(body))
	if !strings.HasPrefix(value, "go1.") || len(value) > 64 {
		return "", errors.New("invalid Go version")
	}
	return value, nil
}

func validateMainPackage(directory, packageName string) error {
	command := exec.Command("go", "list", "-f", "{{.Name}}", packageName)
	command.Dir = directory
	command.Env = controlledGoEnvironment()
	body, err := command.Output()
	if err != nil || strings.TrimSpace(string(body)) != "main" {
		return errors.New("not a main package")
	}
	return nil
}

func controlledGoEnvironment() []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "TMPDIR": true, "GOCACHE": true, "GOMODCACHE": true, "GOPATH": true,
		"GOPROXY": true, "GOSUMDB": true, "GOPRIVATE": true, "GONOPROXY": true, "GONOSUMDB": true,
		"CGO_ENABLED": true, "CC": true, "CXX": true, "PKG_CONFIG_PATH": true,
	}
	result := make([]string, 0, len(allowed)+4)
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && allowed[key] {
			result = append(result, item)
		}
	}
	return append(result, "GOENV=off", "GOFLAGS=", "GOTOOLCHAIN=local", "GOWORK=off")
}

func readRegular(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "bounded-input")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open failed")
	}
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Size < 0 || stat.Size > limit {
		return nil, errors.New("input is not a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("input exceeds limit")
	}
	return body, nil
}

func publish(root *safefs.Root, name string, body []byte) error {
	temp := "." + name + ".tmp"
	file, err := root.CreateExclusive(temp, 0o600)
	if err != nil {
		return err
	}
	fail := func(cause error) error {
		_ = file.Close()
		_ = root.Remove(temp)
		return cause
	}
	if _, err := file.Write(body); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(temp)
		return err
	}
	publication, err := root.PublishNoReplace(temp, name)
	if err != nil && !publication.Visible {
		_ = root.Remove(temp)
		return err
	}
	return nil
}

func hashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
