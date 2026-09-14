// Package config loads config.yaml, which is the single source of truth (credentials
// included); .env only supplies backward-compatible defaults.
//
// Read and written through a YAML node tree so comments survive: after the console edits the
// configuration, config.yaml is still pleasant to edit by hand. config.yaml holds credentials
// such as api_key, so it is gitignored; config.example.yaml is the committed template.
//
// Everything mutable lives under ONE directory, data/, so that persistent state is a single
// thing to back up, move or mount:
//
//	data/config.yaml           the configuration, written back to by the console
//	data/logs/traces/          full-chain trace records
//	data/auth_sessions.json    sign-in sessions
//	data/api_keys.json         API keys
//	data/github/               the GitHub structure / member cache
//
// That one directory is what a container mounts as a volume -- an image layer is discarded on
// every upgrade, so nothing writable may live inside the image. Each path is still individually
// overridable, for a deployment that wants traces on a bigger disk than the configuration:
//
//	MR_DATA_DIR       the root of all persistent state      (default <root>/data)
//	MR_CONFIG_FILE    the config.yaml to read and write     (default <data>/config.yaml)
//	MR_LOG_DIR        full-chain trace records              (default <data>/logs)
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

var (
	// Root is the repository root: the Go server lives in server-go/, so the shared
	// data/, frontend/ and config.example.yaml sit one level up. Overridable with
	// MR_ROOT for a deployment that unpacks the binary somewhere else.
	Root string

	DataDir      string
	LogDir       string
	ConfigPath   string
	TemplatePath string

	// Backward compatibility: older deployments keep credentials in .env. When config.yaml
	// has no providers, these synthesize the default provider.
	EnvEndpoint   string
	EnvAPIKey     string
	EnvAPIVersion string

	legacyConfigPath string
	legacyLogDir     string
)

func init() {
	Root = resolveRoot()
	loadDotEnv(filepath.Join(Root, ".env"))

	EnvEndpoint = os.Getenv("AZURE_OPENAI_ENDPOINT")
	EnvAPIKey = os.Getenv("AZURE_OPENAI_API_KEY")
	EnvAPIVersion = os.Getenv("AZURE_OPENAI_API_VERSION")
	if EnvAPIVersion == "" {
		EnvAPIVersion = "2024-12-01-preview"
	}

	// DataDir first: the other two default to positions *inside* it, so overriding it alone
	// relocates all persistent state together -- which is what makes a container need exactly
	// one mount point and one variable.
	DataDir = pathFromEnv("MR_DATA_DIR", filepath.Join(Root, "data"))
	LogDir = pathFromEnv("MR_LOG_DIR", filepath.Join(DataDir, "logs"))
	// The committed template, which is also what a missing config.yaml is seeded from. It
	// stays at the repository root: it ships with the code and is never written to.
	TemplatePath = filepath.Join(Root, "config.example.yaml")
	ConfigPath = pathFromEnv("MR_CONFIG_FILE", filepath.Join(DataDir, "config.yaml"))

	// Where these two lived before all state was consolidated under data/.
	legacyConfigPath = filepath.Join(Root, "config.yaml")
	legacyLogDir = filepath.Join(Root, "logs")
}

// resolveRoot finds the repository root: MR_ROOT, else the first ancestor of the executable
// or the working directory that carries config.example.yaml. Falling back to the working
// directory keeps `go run ./cmd/model-router` working from anywhere in the tree.
func resolveRoot() string {
	if raw := strings.TrimSpace(os.Getenv("MR_ROOT")); raw != "" {
		if abs, err := filepath.Abs(expandUser(raw)); err == nil {
			return abs
		}
	}
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		candidates = append(candidates, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	for _, start := range candidates {
		for dir := start; ; {
			if _, err := os.Stat(filepath.Join(dir, "config.example.yaml")); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func pathFromEnv(name, fallback string) string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	abs, err := filepath.Abs(expandUser(raw))
	if err != nil {
		return fallback
	}
	return abs
}

func expandUser(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

// loadDotEnv reads KEY=VALUE lines, never overriding a variable the environment already
// sets -- the same precedence python-dotenv's load_dotenv() applies.
func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if _, set := os.LookupEnv(key); !set {
			_ = os.Setenv(key, value)
		}
	}
}

// MigrateLegacyLayout moves a pre-existing root-level config.yaml / logs/ under data/,
// returning what moved.
//
// This is the one genuinely dangerous part of consolidating the layout: config.yaml holds
// every credential, and without this an upgrade would find the new default path empty, seed
// it from the template, and present a working-looking install whose providers, OAuth app and
// admin list had all silently reverted.
//
// Only ever moves INTO an empty destination, and only when the destination is still at its
// default position under DataDir -- an operator who pointed MR_CONFIG_FILE somewhere
// explicitly is not migrated on top of.
func MigrateLegacyLayout() []string {
	var moved []string
	if ConfigPath == filepath.Join(DataDir, "config.yaml") && !exists(ConfigPath) && isFile(legacyConfigPath) {
		if os.MkdirAll(filepath.Dir(ConfigPath), 0o755) == nil && os.Rename(legacyConfigPath, ConfigPath) == nil {
			moved = append(moved, legacyConfigPath+" -> "+ConfigPath)
		}
	}
	// The traces live one level down, under <log dir>/traces, so that is what moves: it keeps
	// any uvicorn logs an operator redirected into logs/ out of the migration.
	legacyTraces := filepath.Join(legacyLogDir, "traces")
	newTraces := filepath.Join(LogDir, "traces")
	if LogDir == filepath.Join(DataDir, "logs") && isDir(legacyTraces) && !exists(newTraces) {
		if os.MkdirAll(filepath.Dir(newTraces), 0o755) == nil && os.Rename(legacyTraces, newTraces) == nil {
			moved = append(moved, legacyTraces+" -> "+newTraces)
		}
	}
	return moved
}

// EnsureConfigFile creates config.yaml from the template when it does not exist yet,
// reporting whether a file was created.
//
// Called before the first read so that a fresh deployment starts instead of dying on a
// missing file. It matters most in a container: the config lives on a mounted volume that
// starts out empty.
//
// A byte copy rather than a YAML round-trip: the template's comments explain every field,
// and they are the seeded file's documentation.
func EnsureConfigFile() (bool, error) {
	if exists(ConfigPath) {
		return false, nil
	}
	if !exists(TemplatePath) {
		return false, &os.PathError{Op: "open", Path: TemplatePath, Err: os.ErrNotExist}
	}
	if err := os.MkdirAll(filepath.Dir(ConfigPath), 0o755); err != nil {
		return false, err
	}
	data, err := os.ReadFile(TemplatePath)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(ConfigPath, data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
