package pi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	assessmentWorkerID         = "assessment-worker-1"
	processMarkerPrefix        = assessmentWorkerID + "-"
	processMarkerSuffix        = ".json"
	processInstanceFile        = assessmentWorkerID + ".instance"
	processMarkerSchemaVersion = 1
	processShutdownGracePeriod = 500 * time.Millisecond
	processShutdownKillWait    = 500 * time.Millisecond
	processOrphanPollInterval  = 20 * time.Millisecond
	processIdentityTokenBytes  = 16
	processRunTokenBytes       = 32
)

var (
	errProcessNotFound       = errors.New("pi process was not found")
	errOwnershipUnverifiable = errors.New("pi process ownership cannot be verified")
)

// ProcessIdentity is the operating-system identity used to prevent a reused
// PID from being mistaken for a process started by this application.
type ProcessIdentity struct {
	StartTime  string
	Executable string
}

// ProcessInspector is intentionally a small testable seam. It does not expose
// a process handle to the assessment module.
type ProcessInspector interface {
	Inspect(pid int) (ProcessIdentity, error)
}

type processMarker struct {
	SchemaVersion       int    `json:"schemaVersion"`
	ApplicationInstance string `json:"applicationInstance"`
	Worker              string `json:"worker"`
	PID                 int    `json:"pid"`
	ProcessStartTime    string `json:"processStartTime"`
	Executable          string `json:"executable"`
	RunToken            string `json:"runToken"`
}

type activeSubmission struct {
	cancel  context.CancelFunc
	process *managedProcess
	done    chan struct{}
}

type managedProcess struct {
	process     *os.Process
	stdin       io.Closer
	wait        <-chan error
	marker      processMarker
	markerPath  string
	inspector   ProcessInspector
	identity    ProcessIdentity
	inputMu     sync.Mutex
	inputClosed bool
}

func (a *Adapter) ensureRuntime() (string, string, error) {
	a.mu.Lock()
	if a.runtimeReady || a.runtimeErr != nil {
		directory, instance, err := a.runtimeDir, a.instanceID, a.runtimeErr
		a.mu.Unlock()
		return directory, instance, err
	}
	configuredDirectory := a.config.RuntimeDir
	a.mu.Unlock()

	directory, err := resolveRuntimeDirectory(configuredDirectory)
	if err == nil {
		err = ensureRuntimeDirectory(directory)
	}
	instance := ""
	if err == nil {
		instance, err = loadOrCreateInstanceID(directory)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runtimeReady || a.runtimeErr != nil {
		return a.runtimeDir, a.instanceID, a.runtimeErr
	}
	if err != nil {
		a.runtimeErr = fmt.Errorf("prepare pi runtime directory: %w", err)
		return "", "", a.runtimeErr
	}
	a.runtimeDir, a.instanceID, a.runtimeReady = directory, instance, true
	return directory, instance, nil
}

func resolveRuntimeDirectory(configured string) (string, error) {
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("pi runtime directory must be absolute")
		}
		return filepath.Clean(configured), nil
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve pi runtime directory: %w", err)
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("pi runtime directory root must be absolute")
	}
	return filepath.Join(root, "boss-job-agent", "runtime", "pi"), nil
}

func ensureRuntimeDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create pi runtime directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect pi runtime directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("pi runtime directory is not a private directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // The runtime directory must remain owner-only.
		return fmt.Errorf("protect pi runtime directory: %w", err)
	}
	return nil
}

func loadOrCreateInstanceID(directory string) (string, error) {
	path := filepath.Join(directory, processInstanceFile)
	if instanceID, found, err := readExistingInstanceID(path); found || err != nil {
		return instanceID, err
	}
	return createInstanceIDFile(path)
}

func readExistingInstanceID(path string) (string, bool, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", true, fmt.Errorf("pi application instance file is not regular")
		}
		instanceID, readErr := readInstanceID(path)
		return instanceID, true, readErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", true, fmt.Errorf("inspect pi application instance file: %w", err)
	}
	return "", false, nil
}

func createInstanceIDFile(path string) (string, error) {
	instanceID, err := randomHex(processIdentityTokenBytes)
	if err != nil {
		return "", fmt.Errorf("create pi application instance ID: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // The path is under the private Pi runtime directory.
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return readInstanceID(path)
		}
		return "", fmt.Errorf("create pi application instance file: %w", err)
	}
	if _, writeErr := io.WriteString(file, instanceID+"\n"); writeErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write pi application instance file: %w", writeErr)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("sync pi application instance file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close pi application instance file: %w", err)
	}
	return instanceID, nil
}

func readInstanceID(path string) (string, error) {
	contents, err := os.ReadFile(path) //nolint:gosec // The path is under the private Pi runtime directory.
	if err != nil {
		return "", fmt.Errorf("read pi application instance file: %w", err)
	}
	instanceID := strings.TrimSpace(string(contents))
	if !validHexToken(instanceID, processIdentityTokenBytes) {
		return "", fmt.Errorf("pi application instance file contains an invalid ID")
	}
	return instanceID, nil
}

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (a *Adapter) Recover(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, instanceID, err := a.ensureRuntime()
	if err != nil {
		return err
	}
	a.mu.Lock()
	active := a.active != nil
	a.mu.Unlock()
	if active {
		return fmt.Errorf("recover pi processes: an assessment request is still active")
	}
	markers, err := listProcessMarkers(directory)
	if err != nil {
		return fmt.Errorf("list pi process markers: %w", err)
	}
	var recoveryErr error
	for _, path := range markers {
		recoveryErr = errors.Join(recoveryErr, a.recoverMarker(ctx, path, instanceID))
	}
	return recoveryErr
}

func (a *Adapter) recoverMarker(ctx context.Context, path, instanceID string) error {
	marker, err := readProcessMarker(path)
	if errors.Is(err, os.ErrNotExist) {
		// Another lifecycle operation may have removed this marker after the
		// directory listing confirmed that it existed.
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect pi process marker %s: %w", path, err)
	}
	if err := validateProcessMarker(marker, instanceID); err != nil {
		return fmt.Errorf("verify pi process marker %s: %w", path, err)
	}
	identity, err := a.inspector.Inspect(marker.PID)
	if errors.Is(err, errProcessNotFound) {
		return removeProcessMarker(path)
	}
	if err != nil {
		return ownershipError(marker, err)
	}
	if !sameProcessIdentity(marker, identity) {
		return ownershipError(marker, nil)
	}
	if err := terminateOrphan(ctx, marker, a.inspector); err != nil {
		return err
	}
	return removeProcessMarker(path)
}

func listProcessMarkers(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), processMarkerPrefix) || !strings.HasSuffix(entry.Name(), processMarkerSuffix) {
			continue
		}
		paths = append(paths, filepath.Join(directory, entry.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

func readProcessMarker(path string) (processMarker, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return processMarker{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return processMarker{}, fmt.Errorf("marker is not a regular file")
	}
	file, err := os.Open(path) //nolint:gosec // Marker paths come from the private runtime directory listing.
	if err != nil {
		return processMarker{}, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var marker processMarker
	if err := decoder.Decode(&marker); err != nil {
		return processMarker{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return processMarker{}, fmt.Errorf("marker has trailing JSON")
		}
		return processMarker{}, err
	}
	return marker, nil
}

func validateProcessMarker(marker processMarker, instanceID string) error {
	if marker.SchemaVersion != processMarkerSchemaVersion {
		return fmt.Errorf("unsupported marker schema version %d", marker.SchemaVersion)
	}
	if marker.ApplicationInstance != instanceID {
		return fmt.Errorf("application instance does not match")
	}
	if marker.Worker != assessmentWorkerID || marker.PID <= 0 || marker.ProcessStartTime == "" || marker.Executable == "" {
		return fmt.Errorf("marker identity is incomplete")
	}
	if !filepath.IsAbs(marker.Executable) || !validHexToken(marker.RunToken, processRunTokenBytes) {
		return fmt.Errorf("marker identity is invalid")
	}
	return nil
}

func validHexToken(value string, size int) bool {
	if len(value) != size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sameProcessIdentity(marker processMarker, identity ProcessIdentity) bool {
	return marker.ProcessStartTime == identity.StartTime && marker.Executable == identity.Executable
}

func ownershipError(marker processMarker, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: PID %d identity differs from the marker", errOwnershipUnverifiable, marker.PID)
	}
	return fmt.Errorf("%w: PID %d: %w", errOwnershipUnverifiable, marker.PID, cause)
}

func terminateOrphan(ctx context.Context, marker processMarker, inspector ProcessInspector) error {
	if err := verifyOrphan(marker, inspector); errors.Is(err, errProcessNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	process, err := os.FindProcess(marker.PID)
	if err != nil {
		return ownershipError(marker, err)
	}
	if err := interruptProcess(process, marker, inspector); err != nil {
		return err
	}
	if err := waitForOrphanExit(ctx, marker, inspector, processShutdownGracePeriod); errors.Is(err, errProcessNotFound) {
		return nil
	} else if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return killOrphan(ctx, process, marker, inspector)
}

func verifyOrphan(marker processMarker, inspector ProcessInspector) error {
	identity, err := inspector.Inspect(marker.PID)
	if err != nil {
		return err
	}
	if !sameProcessIdentity(marker, identity) {
		return ownershipError(marker, nil)
	}
	return nil
}

func interruptProcess(process *os.Process, marker processMarker, inspector ProcessInspector) error {
	if err := verifyOrphan(marker, inspector); errors.Is(err, errProcessNotFound) {
		return nil
	} else if err != nil {
		return ownershipError(marker, err)
	}
	if err := process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// The process may have exited between identity verification and the signal.
		if _, inspectErr := inspector.Inspect(marker.PID); !errors.Is(inspectErr, errProcessNotFound) {
			return ownershipError(marker, err)
		}
	}
	return nil
}

func killOrphan(ctx context.Context, process *os.Process, marker processMarker, inspector ProcessInspector) error {
	if err := verifyOrphan(marker, inspector); errors.Is(err, errProcessNotFound) {
		return nil
	} else if err != nil {
		return ownershipError(marker, err)
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return ownershipError(marker, err)
	}
	if err := waitForOrphanExit(ctx, marker, inspector, processShutdownKillWait); err != nil && !errors.Is(err, errProcessNotFound) {
		return fmt.Errorf("terminate verified orphaned pi PID %d: %w", marker.PID, err)
	}
	return nil
}

func waitForOrphanExit(ctx context.Context, marker processMarker, inspector ProcessInspector, wait time.Duration) error {
	waitContext, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(processOrphanPollInterval)
	defer ticker.Stop()
	for {
		_, err := inspector.Inspect(marker.PID)
		if errors.Is(err, errProcessNotFound) {
			return errProcessNotFound
		}
		if err != nil {
			return ownershipError(marker, err)
		}
		select {
		case <-waitContext.Done():
			return waitContext.Err()
		case <-ticker.C:
		}
	}
}

func writeProcessMarker(directory string, marker processMarker) (string, error) {
	path := filepath.Join(directory, processMarkerPrefix+marker.RunToken+processMarkerSuffix)
	file, err := os.CreateTemp(directory, "."+processMarkerPrefix+"*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := file.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", err
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(marker); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	removeTemporary = false
	return path, nil
}

func removeProcessMarker(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pi process marker %s: %w", path, err)
	}
	return nil
}

func (a *Adapter) attachProcess(
	commandProcess *os.Process,
	stdin io.Closer,
	wait <-chan error,
	directory string,
	instanceID string,
	runToken string,
) (*managedProcess, error) {
	identity, err := a.inspector.Inspect(commandProcess.Pid)
	if err != nil {
		markerPath, markerErr := writeProcessMarker(directory, processMarker{
			SchemaVersion:       processMarkerSchemaVersion,
			ApplicationInstance: instanceID,
			Worker:              assessmentWorkerID,
			PID:                 commandProcess.Pid,
			ProcessStartTime:    "unverified",
			Executable:          filepath.Join(directory, ".unverified"),
			RunToken:            runToken,
		})
		if markerErr != nil {
			return nil, errors.Join(
				fmt.Errorf("verify started pi process: %w", err),
				fmt.Errorf("write unverifiable pi process marker: %w", markerErr),
			)
		}
		return nil, fmt.Errorf("verify started pi process: %w; marker retained at %s", err, markerPath)
	}
	marker := processMarker{
		SchemaVersion:       processMarkerSchemaVersion,
		ApplicationInstance: instanceID,
		Worker:              assessmentWorkerID,
		PID:                 commandProcess.Pid,
		ProcessStartTime:    identity.StartTime,
		Executable:          identity.Executable,
		RunToken:            runToken,
	}
	markerPath, err := writeProcessMarker(directory, marker)
	if err != nil {
		managed := &managedProcess{
			process: commandProcess, stdin: stdin, wait: wait, marker: marker,
			inspector: a.inspector, identity: identity,
		}
		_, cleanupErr, _ := a.shutdownManagedProcess(managed)
		return nil, errors.Join(
			fmt.Errorf("write pi process marker: %w", err),
			cleanupErr,
		)
	}
	managed := &managedProcess{
		process: commandProcess, stdin: stdin, wait: wait, marker: marker,
		markerPath: markerPath, inspector: a.inspector, identity: identity,
	}
	a.mu.Lock()
	if a.active != nil {
		a.active.process = managed
	}
	a.mu.Unlock()
	return managed, nil
}

func (p *managedProcess) closeInput() error {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	if p.inputClosed {
		return nil
	}
	p.inputClosed = true
	return p.stdin.Close()
}

func (p *managedProcess) shutdown() (error, error, bool) {
	_ = p.closeInput()
	if waitErr, done := awaitProcess(p.wait, processShutdownGracePeriod); done {
		return waitErr, p.removeMarker(), false
	}
	return p.escalateShutdown()
}

func (p *managedProcess) escalateShutdown() (error, error, bool) {
	if err := p.verifyCurrentProcess(); err != nil {
		return p.finishAfterIdentityError(err, false)
	}
	_ = p.process.Signal(os.Interrupt)
	if waitErr, done := awaitProcess(p.wait, processShutdownGracePeriod); done {
		return waitErr, p.removeMarker(), true
	}
	if err := p.verifyCurrentProcess(); err != nil {
		return p.finishAfterIdentityError(err, true)
	}
	if err := p.process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return nil, ownershipError(p.marker, err), true
	}
	if waitErr, done := awaitProcess(p.wait, processShutdownKillWait); done {
		return waitErr, p.removeMarker(), true
	}
	return nil, fmt.Errorf("terminate verified Pi PID %d: process did not exit", p.process.Pid), true
}

func (p *managedProcess) finishAfterIdentityError(err error, forced bool) (error, error, bool) {
	if errors.Is(err, errProcessNotFound) {
		if waitErr, done := awaitProcess(p.wait, processShutdownKillWait); done {
			return waitErr, p.removeMarker(), forced
		}
	}
	return nil, err, forced
}

func (a *Adapter) shutdownManagedProcess(process *managedProcess) (error, error, bool) {
	return process.shutdown()
}

func awaitProcess(wait <-chan error, timeout time.Duration) (error, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case err := <-wait:
		return err, true
	case <-ctx.Done():
		return nil, false
	}
}

func (p *managedProcess) verifyCurrentProcess() error {
	identity, err := p.inspector.Inspect(p.process.Pid)
	if err != nil {
		if errors.Is(err, errProcessNotFound) {
			return err
		}
		return ownershipError(p.marker, err)
	}
	if identity != p.identity {
		return ownershipError(p.marker, nil)
	}
	return nil
}

func (p *managedProcess) removeMarker() error {
	if p.markerPath == "" {
		return nil
	}
	return removeProcessMarker(p.markerPath)
}
