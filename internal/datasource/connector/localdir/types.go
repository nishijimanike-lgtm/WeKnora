// Package localdir implements the local-directory data source connector.
//
// It syncs files from a directory on the WeKnora server's own filesystem into
// a knowledge base. Operators allowlist the directories the connector may
// read via WEKNORA_LOCAL_DATASOURCE_ALLOWED_ROOTS (comma-separated); the
// connector is disabled while the variable is empty. Each data source picks a
// root_path under one of the allowlisted directories, and the sub-folders
// under that root distinguish data of different natures or origins — users
// select which sub-folders (or individual files) to sync in the resource
// picker, and the knowledge-base folder tree mirrors the directory layout.
//
// Capabilities:
//   - Hierarchical, lazily-loaded resource tree (folders + files).
//   - Streaming sync with per-selection checkpoints (StreamingConnector).
//   - Incremental sync via a size+mtime signature per file.
//   - Deletion sync: files removed from a still-selected folder are removed
//     from the knowledge base (subject to the data source's sync_deletions
//     flag, enforced by the service).
//
// Safety:
//   - root_path must resolve (after symlink evaluation) inside an allowlisted
//     root; every listed/fetched path is re-checked for containment.
//   - Symbolic links are never followed during listing or sync.
//   - Hidden entries (dot-prefixed) are skipped unless explicitly enabled.
//   - Files above a size cap are skipped instead of read into memory.
package localdir

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

// AllowedRootsEnvVar lists the server directories the local-directory
// connector may read from, separated by commas. When unset or empty the
// connector is disabled: Validate/List/sync all fail with a clear error so
// the feature is strictly opt-in for operators.
const AllowedRootsEnvVar = "WEKNORA_LOCAL_DATASOURCE_ALLOWED_ROOTS"

// DefaultMaxFileSizeMB caps how large a single synced file may be. Larger
// files are skipped (with a warning) instead of being read into memory.
const DefaultMaxFileSizeMB = 100

// Config holds local-directory connector configuration. All fields live in
// DataSourceConfig.Settings (nothing about a local path is secret). The
// credentials map may carry root_path transiently on the validate-credentials
// path, mirroring how RSS passes feed_urls.
type Config struct {
	// RootPath is the absolute directory to sync from. Required.
	RootPath string `json:"root_path"`

	// IncludeHidden also syncs dot-prefixed files and directories.
	IncludeHidden bool `json:"include_hidden"`

	// MaxFileSizeMB overrides DefaultMaxFileSizeMB when > 0.
	MaxFileSizeMB int `json:"max_file_size_mb"`

	// FileExtensions optionally restricts sync to these extensions
	// (comma/space/newline separated, dot optional, e.g. "pdf, .md").
	// Empty means the built-in supported-document set.
	FileExtensions string `json:"file_extensions"`
}

// rawConfig is the wire form of Config. Numeric and boolean fields accept
// JSON numbers/bools as well as strings ("50", "true") because different
// clients (UI form inputs, API playground, CLI) serialize inputs loosely.
type rawConfig struct {
	RootPath       string      `json:"root_path"`
	IncludeHidden  interface{} `json:"include_hidden"`
	MaxFileSizeMB  interface{} `json:"max_file_size_mb"`
	FileExtensions string      `json:"file_extensions"`
}

func (r *rawConfig) into(dst *Config) {
	if root := strings.TrimSpace(r.RootPath); root != "" {
		dst.RootPath = root
	}
	if v, ok := coerceBool(r.IncludeHidden); ok {
		dst.IncludeHidden = v
	}
	if v, ok := coerceInt(r.MaxFileSizeMB); ok {
		dst.MaxFileSizeMB = v
	}
	if ext := strings.TrimSpace(r.FileExtensions); ext != "" {
		dst.FileExtensions = ext
	}
}

func coerceBool(v interface{}) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		if parsed, err := strconv.ParseBool(strings.TrimSpace(b)); err == nil {
			return parsed, true
		}
	}
	return false, false
}

func coerceInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i, true
		}
	}
	return 0, false
}

// parseConfig extracts and validates local-directory configuration. Settings
// win over credentials so the persisted (editable) values always prevail;
// credentials only carry root_path transiently on validate-credentials.
func parseConfig(config *types.DataSourceConfig) (*Config, error) {
	if config == nil {
		return nil, fmt.Errorf("%w: config is nil", datasource.ErrInvalidConfig)
	}
	var cfg Config
	if len(config.Credentials) > 0 {
		credBytes, err := json.Marshal(config.Credentials)
		if err != nil {
			return nil, fmt.Errorf("marshal credentials: %w", err)
		}
		var raw rawConfig
		if err := json.Unmarshal(credBytes, &raw); err != nil {
			return nil, fmt.Errorf("parse local_dir credentials: %w", err)
		}
		raw.into(&cfg)
	}
	if len(config.Settings) > 0 {
		setBytes, err := json.Marshal(config.Settings)
		if err != nil {
			return nil, fmt.Errorf("marshal settings: %w", err)
		}
		var raw rawConfig
		if err := json.Unmarshal(setBytes, &raw); err != nil {
			return nil, fmt.Errorf("parse local_dir settings: %w", err)
		}
		raw.into(&cfg)
	}
	cfg.RootPath = strings.TrimSpace(cfg.RootPath)
	if cfg.RootPath == "" {
		return nil, fmt.Errorf("%w: root_path is required", datasource.ErrInvalidConfig)
	}
	return &cfg, nil
}

// maxFileSizeBytes returns the effective per-file size cap in bytes.
func (c *Config) maxFileSizeBytes() int64 {
	mb := c.MaxFileSizeMB
	if mb <= 0 {
		mb = DefaultMaxFileSizeMB
	}
	return int64(mb) * 1024 * 1024
}

// extensionFilter returns the set of allowed lower-cased extensions
// (with leading dot). nil means "use defaultSupportedExtensions".
func (c *Config) extensionFilter() map[string]struct{} {
	raw := strings.FieldsFunc(c.FileExtensions, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == ';' || r == '、'
	})
	if len(raw) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(raw))
	for _, ext := range raw {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		set[ext] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// isSupportedFile reports whether the file should be synced, given the
// configured (or default) extension set.
func (c *Config) isSupportedFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	if filter := c.extensionFilter(); filter != nil {
		_, ok := filter[ext]
		return ok
	}
	_, ok := defaultSupportedExtensions[ext]
	return ok
}

// defaultSupportedExtensions limits local-directory sync to formats the
// knowledge import pipeline can process. Mirrors the GitLab connector's
// list, which serves the same "arbitrary files, document-centric pipeline"
// role.
var defaultSupportedExtensions = map[string]struct{}{
	".pdf": {}, ".txt": {}, ".docx": {}, ".doc": {}, ".epub": {},
	".html": {}, ".htm": {}, ".mhtml": {}, ".md": {}, ".markdown": {}, ".mdx": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {},
	".csv": {}, ".xlsx": {}, ".xls": {}, ".pptx": {}, ".ppt": {}, ".json": {},
	".mp3": {}, ".wav": {}, ".m4a": {}, ".flac": {}, ".ogg": {},
}

// resolveCanonical resolves p to an absolute, symlink-evaluated path.
// EvalSymlinks failures fall back to lexical cleaning so not-yet-created
// paths still produce a stable, comparable form.
func resolveCanonical(p string) string {
	abs, err := filepath.Abs(strings.TrimSpace(p))
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// containsPath reports whether child equals parent or lives beneath it.
// Both inputs must already be canonical (clean, absolute, symlinks resolved).
func containsPath(parent, child string) bool {
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// allowedRoots returns the operator-allowlisted directories in canonical
// form. An empty list means the local-directory connector is disabled.
func allowedRoots() []string {
	raw := strings.Split(os.Getenv(AllowedRootsEnvVar), ",")
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		out = append(out, resolveCanonical(entry))
	}
	return out
}

// disabledError is the error surfaced while the allowlist is empty.
func disabledError() error {
	return fmt.Errorf(
		"%w: local directory data sources are disabled; the operator must set %s to a comma-separated list of allowed directories",
		datasource.ErrInvalidConfig, AllowedRootsEnvVar,
	)
}

// resolveRoot validates the configured root path against the allowlist and
// returns it in canonical form. The returned path is guaranteed to live
// under one of the allowlisted roots (symlinks evaluated on both sides).
func resolveRoot(cfg *Config) (string, error) {
	roots := allowedRoots()
	if len(roots) == 0 {
		return "", disabledError()
	}
	candidate := resolveCanonical(cfg.RootPath)
	for _, root := range roots {
		if containsPath(root, candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"%w: root_path %q is not under any allowed directory (%s=%q)",
		datasource.ErrInvalidConfig, cfg.RootPath, AllowedRootsEnvVar, os.Getenv(AllowedRootsEnvVar),
	)
}
