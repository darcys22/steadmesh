package client

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Environment variable and default file locations shared by seat-runner and
// steadmesh-tools (docs/architecture.html#seat-pod).
const (
	EnvPlatformURL    = "STEADMESH_PLATFORM_URL"
	EnvTokenFile      = "STEADMESH_TOKEN_FILE"
	EnvGeneration     = "STEADMESH_GENERATION"
	EnvGenerationFile = "STEADMESH_GENERATION_FILE"
	EnvExecution      = "STEADMESH_EXECUTION"
	EnvExecutionFile  = "STEADMESH_EXECUTION_FILE"
	EnvRunnerDir      = "STEADMESH_RUNNER_DIR"

	DefaultTokenFile = "/var/run/steadmesh/token"
	DefaultRunnerDir = "/seat/runner"
)

// RunnerDir returns STEADMESH_RUNNER_DIR or /seat/runner.
func RunnerDir() string {
	if d := os.Getenv(EnvRunnerDir); d != "" {
		return d
	}
	return DefaultRunnerDir
}

// GenerationFile returns the path of the file the runner writes its lease
// generation to.
func GenerationFile() string {
	if f := os.Getenv(EnvGenerationFile); f != "" {
		return f
	}
	return strings.TrimRight(RunnerDir(), "/") + "/generation"
}

// ExecutionFile returns the path of the file holding the current execution id.
func ExecutionFile() string {
	if f := os.Getenv(EnvExecutionFile); f != "" {
		return f
	}
	return strings.TrimRight(RunnerDir(), "/") + "/execution"
}

// GenerationFromEnvOrFile returns a source that prefers STEADMESH_GENERATION and
// otherwise re-reads the generation file on every call.
func GenerationFromEnvOrFile() func() int64 {
	return func() int64 {
		if v := os.Getenv(EnvGeneration); v != "" {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
		b, err := os.ReadFile(GenerationFile())
		if err != nil {
			return 0
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return n
	}
}

// ExecutionFromEnvOrFile returns a source that prefers STEADMESH_EXECUTION and
// otherwise re-reads the execution file on every call.
func ExecutionFromEnvOrFile() func() string {
	return func() string {
		if v := os.Getenv(EnvExecution); v != "" {
			return strings.TrimSpace(v)
		}
		b, err := os.ReadFile(ExecutionFile())
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
}

// FromEnv builds a client from the seat environment. The generation and
// execution id come from env vars or the runner's files.
func FromEnv(userAgent string) (*Client, error) {
	base := os.Getenv(EnvPlatformURL)
	if base == "" {
		return nil, errors.New("STEADMESH_PLATFORM_URL is not set")
	}
	tf := os.Getenv(EnvTokenFile)
	if tf == "" {
		tf = DefaultTokenFile
	}
	return New(Config{
		BaseURL:    base,
		TokenFile:  tf,
		Generation: GenerationFromEnvOrFile(),
		Execution:  ExecutionFromEnvOrFile(),
		UserAgent:  userAgent,
	})
}

// WriteFileAtomic writes data to path via a temporary file and rename, so
// concurrent readers never see a partial value.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
