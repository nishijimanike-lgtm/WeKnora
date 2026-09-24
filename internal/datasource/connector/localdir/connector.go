package localdir

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// Compile-time proofs that *Connector satisfies the connector interfaces.
var (
	_ datasource.Connector          = (*Connector)(nil)
	_ datasource.StreamingConnector = (*Connector)(nil)
)

// Connector implements the local-directory data source connector. It is
// stateless: every call resolves the configured root against the operator
// allowlist again, so allowlist changes take effect without a restart.
type Connector struct{}

// NewConnector creates a new local-directory connector.
func NewConnector() *Connector { return &Connector{} }

// Type returns the connector type identifier.
func (c *Connector) Type() string { return types.ConnectorTypeLocalDir }

// Validate checks that the connector is enabled, the configured root exists,
// is a directory, and is readable.
func (c *Connector) Validate(ctx context.Context, config *types.DataSourceConfig) error {
	cfg, err := parseConfig(config)
	if err != nil {
		return err
	}
	root, err := resolveRoot(cfg)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("%w: root path %q is not accessible: %v", datasource.ErrInvalidConfig, cfg.RootPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: root path %q is not a directory", datasource.ErrInvalidConfig, cfg.RootPath)
	}
	if _, err := os.ReadDir(root); err != nil {
		return fmt.Errorf("%w: root path %q cannot be listed: %v", datasource.ErrInvalidConfig, cfg.RootPath, err)
	}
	return nil
}

// ListResources lists one level of the directory tree. parentID == "" lists
// the entries directly under the configured root; a non-empty parentID (a
// root-relative slash path) lists that directory's entries. Symlinks are
// never listed; unsupported files are omitted because they cannot be ingested.
func (c *Connector) ListResources(
	ctx context.Context, config *types.DataSourceConfig, parentID string,
) ([]types.Resource, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	root, err := resolveRoot(cfg)
	if err != nil {
		return nil, err
	}

	dir := root
	if parentID != "" {
		dir, err = secutils.SafeJoinUnderBase(root, filepath.FromSlash(parentID))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", datasource.ErrInvalidConfig, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list directory %q: %w", displayRel(root, dir), err)
	}

	out := make([]types.Resource, 0, len(entries))
	for _, entry := range entries {
		// Symlinks are skipped entirely: following them could leave the
		// allowlisted root.
		if entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if isHiddenName(entry.Name()) && !cfg.IncludeHidden {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			logger.Warnf(ctx, "[LocalDir] stat %s/%s failed: %v", dir, entry.Name(), err)
			continue
		}
		rel := toSlashPath(displayRel(root, filepath.Join(dir, entry.Name())))
		if entry.IsDir() {
			out = append(out, types.Resource{
				ExternalID:  rel,
				Name:        entry.Name(),
				Type:        "directory",
				ParentID:    normalizeSlashes(parentID),
				HasChildren: true,
				ModifiedAt:  info.ModTime(),
			})
			continue
		}
		if !cfg.isSupportedFile(entry.Name()) {
			continue
		}
		out = append(out, types.Resource{
			ExternalID: rel,
			Name:       entry.Name(),
			Type:       "file",
			ParentID:   normalizeSlashes(parentID),
			ModifiedAt: info.ModTime(),
			Metadata: map[string]interface{}{
				"size": info.Size(),
			},
		})
	}

	// Directories first, then case-insensitive by name — the same ordering
	// users expect from a file manager.
	sort.Slice(out, func(i, j int) bool {
		di, dj := out[i].Type == "directory", out[j].Type == "directory"
		if di != dj {
			return di
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// ResolveResourceAncestors returns every ancestor directory of the given
// selections so the lazily-loaded picker can reveal deep pre-existing
// selections. Purely lexical — no filesystem access needed.
func (c *Connector) ResolveResourceAncestors(
	ctx context.Context, config *types.DataSourceConfig, resourceIDs []string,
) ([]string, error) {
	set := make(map[string]struct{})
	for _, id := range resourceIDs {
		cleaned := normalizeSlashes(id)
		if cleaned == "" {
			continue
		}
		parts := strings.Split(cleaned, "/")
		for i := 1; i < len(parts); i++ {
			set[strings.Join(parts[:i], "/")] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for ancestor := range set {
		out = append(out, ancestor)
	}
	sort.Strings(out)
	return out, nil
}

// FetchAll performs a full (non-streaming) sync of the selected resources.
// The service prefers FetchStream for this connector; this remains the
// compatibility path required by the base Connector interface.
func (c *Connector) FetchAll(
	ctx context.Context, config *types.DataSourceConfig, resourceIDs []string,
) ([]types.FetchedItem, error) {
	var out []types.FetchedItem
	h := collectHandler(func(item types.FetchedItem) error {
		out = append(out, item)
		return nil
	})
	if _, err := c.sync(ctx, config, resourceIDs, nil, h); err != nil {
		return out, err
	}
	return out, nil
}

// FetchIncremental returns items changed since the previous cursor plus
// deletions for files removed from still-selected directories.
func (c *Connector) FetchIncremental(
	ctx context.Context, config *types.DataSourceConfig, cursor *types.SyncCursor,
) ([]types.FetchedItem, *types.SyncCursor, error) {
	if config == nil {
		return nil, nil, fmt.Errorf("%w: config is nil", datasource.ErrInvalidConfig)
	}
	var out []types.FetchedItem
	h := collectHandler(func(item types.FetchedItem) error {
		out = append(out, item)
		return nil
	})
	next, err := c.sync(ctx, config, config.ResourceIDs, cursor, h)
	if err != nil && next == nil {
		return nil, nil, err
	}
	return out, next, err
}

// FetchStream walks the selected directories, emitting each changed file as
// it is read and checkpointing after every top-level selection, so a large
// directory sync stays memory-bounded and resumable.
func (c *Connector) FetchStream(
	ctx context.Context, config *types.DataSourceConfig,
	cursor *types.SyncCursor, h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	if config == nil {
		return nil, fmt.Errorf("%w: config is nil", datasource.ErrInvalidConfig)
	}
	return c.sync(ctx, config, config.ResourceIDs, cursor, h)
}

// syncState carries the mutable state of one sync run.
type syncState struct {
	cfg         *Config
	root        string
	incremental bool
	prev, next  *cursorState

	// seen: eligible files observed on disk this run (synced or unchanged).
	seen map[string]struct{}
	// existsButSkipped: files present on disk but ineligible this run
	// (extension filtered out or over the size cap). Their previous cursor
	// entries are carried over so they are neither re-ingested nor deleted.
	existsButSkipped map[string]struct{}
	// unsafePrefixes: subtrees that could not be fully listed; deletions
	// below them are suppressed so a transient read error cannot wipe
	// still-existing knowledge.
	unsafePrefixes []string
	warnings       []string

	emit func(types.FetchedItem) error
}

// sync is the shared full/incremental/streaming implementation. cursor == nil
// means a full sync (no change detection, no deletions).
func (c *Connector) sync(
	ctx context.Context,
	config *types.DataSourceConfig,
	resourceIDs []string,
	cursor *types.SyncCursor,
	h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	root, err := resolveRoot(cfg)
	if err != nil {
		return nil, err
	}

	st := &syncState{
		cfg:              cfg,
		root:             root,
		prev:             parseCursorState(cursor),
		next:             &cursorState{Root: root, Files: make(map[string]fileSig)},
		seen:             make(map[string]struct{}),
		existsButSkipped: make(map[string]struct{}),
		emit:             func(item types.FetchedItem) error { return h.Emit(ctx, item) },
	}
	st.incremental = st.prev != nil
	// A changed root_path invalidates the signature baseline: identical
	// relative paths in a different directory must be re-ingested fully.
	if st.prev != nil && st.prev.Root != root {
		logger.Infof(ctx, "[LocalDir] root changed since last sync (%q -> %q); treating as full", st.prev.Root, root)
		st.prev = nil
		st.incremental = false
	}

	selections := normalizeSelections(resourceIDs)

	for _, sel := range selections {
		abs, joinErr := secutils.SafeJoinUnderBase(root, filepath.FromSlash(sel))
		if joinErr != nil {
			st.warnings = append(st.warnings, fmt.Sprintf("selection %q rejected: %v", sel, joinErr))
			logger.Warnf(ctx, "[LocalDir] rejecting selection %q: %v", sel, joinErr)
			continue
		}
		info, statErr := os.Stat(abs)
		switch {
		case statErr == nil && info.IsDir():
			if walkErr := c.walkDir(ctx, st, abs, sel); walkErr != nil {
				st.warnings = append(st.warnings, fmt.Sprintf("selection %q: %v", sel, walkErr))
			}
		case statErr == nil:
			if visitErr := c.visitFile(ctx, st, abs, sel, info); visitErr != nil {
				return nil, visitErr
			}
		case os.IsNotExist(statErr):
			// Selection removed at the source: its previously-synced files
			// reconcile as deletions below via the seen/prev diff.
			logger.Infof(ctx, "[LocalDir] selection %q no longer exists", sel)
		default:
			// Stat failed for another reason (permissions, I/O): we cannot
			// tell what is still there, so suppress deletions underneath.
			st.warnings = append(st.warnings, fmt.Sprintf("selection %q not accessible: %v", sel, statErr))
			logger.Warnf(ctx, "[LocalDir] selection %q not accessible: %v", sel, statErr)
			st.unsafePrefixes = append(st.unsafePrefixes, sel)
		}

		if err := h.Checkpoint(ctx, buildSyncCursor(st.next)); err != nil {
			return nil, err
		}
	}

	// Deletion reconciliation: prev entries inside a current selection that
	// were not seen on disk become deletions; entries outside every selection
	// (the user narrowed the scope) are carried over untouched so narrowing
	// the scope never deletes knowledge.
	if st.incremental {
		for rel, sig := range st.prev.Files {
			if _, ok := st.seen[rel]; ok {
				continue
			}
			if _, skipped := st.existsButSkipped[rel]; skipped {
				st.next.Files[rel] = sig
				continue
			}
			if !coveredByAnySelection(rel, selections) {
				st.next.Files[rel] = sig
				continue
			}
			if underAnyPrefix(rel, st.unsafePrefixes) {
				st.next.Files[rel] = sig
				continue
			}
			if err := st.emit(deletedItem(rel)); err != nil {
				return nil, err
			}
		}
	}

	if len(st.warnings) > 0 && len(st.next.Files) == 0 && len(st.seen) == 0 {
		return buildSyncCursor(st.next), fmt.Errorf("local directory sync failed: %s", strings.Join(st.warnings, "; "))
	}
	if len(st.warnings) > 0 {
		logger.Warnf(ctx, "[LocalDir] sync completed with warnings: %s", strings.Join(st.warnings, "; "))
	}
	return buildSyncCursor(st.next), nil
}

// walkDir recursively emits supported files below absDir. selRel is the
// root-relative slash path of the selection (SourceResourceID attribution).
func (c *Connector) walkDir(ctx context.Context, st *syncState, absDir, selRel string) error {
	return filepath.WalkDir(absDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// WalkDir surfaces per-entry errors. A directory it could not
			// enter becomes an unsafe prefix (no deletions below it); other
			// entry errors are skipped individually.
			if d != nil && d.IsDir() && p != absDir {
				st.unsafePrefixes = append(st.unsafePrefixes, toSlashPath(displayRel(st.root, p)))
				logger.Warnf(ctx, "[LocalDir] cannot list %s: %v", p, err)
				return nil
			}
			if p == absDir {
				return err
			}
			logger.Warnf(ctx, "[LocalDir] skipping %s: %v", p, err)
			return nil
		}
		// Never follow symlinks; WalkDir does not descend into symlinked
		// directories, and linked files are skipped outright.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		// The selection root itself is always honored, even if hidden.
		if p != absDir && isHiddenName(d.Name()) && !st.cfg.IncludeHidden {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			logger.Warnf(ctx, "[LocalDir] stat %s failed: %v", p, infoErr)
			return nil
		}
		return c.visitFile(ctx, st, p, selRel, info)
	})
}

// visitFile syncs one regular file: unchanged files are cursor carry-overs,
// changed/new files are read and emitted, ineligible files are recorded so a
// previous copy is neither re-ingested nor deleted.
func (c *Connector) visitFile(
	ctx context.Context, st *syncState, absPath, selectionRel string, info fs.FileInfo,
) error {
	rel := toSlashPath(displayRel(st.root, absPath))

	if !st.cfg.isSupportedFile(info.Name()) || info.Size() > st.cfg.maxFileSizeBytes() {
		if info.Size() > st.cfg.maxFileSizeBytes() {
			logger.Warnf(ctx, "[LocalDir] skipping %s: %d bytes exceeds the %d MB limit",
				absPath, info.Size(), st.cfg.maxFileSizeBytes()/(1024*1024))
		}
		st.existsButSkipped[rel] = struct{}{}
		return nil
	}

	sig := fileSig{Size: info.Size(), MTime: info.ModTime().Unix()}
	st.seen[rel] = struct{}{}

	if st.incremental && st.prev != nil && st.prev.Files[rel] == sig {
		// Unchanged since the last sync: keep the baseline, skip the read.
		st.next.Files[rel] = sig
		return nil
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		// Do not record the signature: the next sync retries the read. Surface
		// the failure as a placeholder item so the sync log explains the gap.
		logger.Warnf(ctx, "[LocalDir] read %s failed: %v", absPath, err)
		delete(st.seen, rel)
		return st.emit(types.FetchedItem{
			ExternalID:       rel,
			Title:            rel,
			SourceResourceID: selectionRel,
			Metadata: map[string]string{
				"channel": types.ChannelLocalDir,
				"error":   fmt.Sprintf("read file: %v", err),
			},
		})
	}

	st.next.Files[rel] = sig
	return st.emit(types.FetchedItem{
		ExternalID:       rel,
		Title:            rel,
		Content:          content,
		ContentType:      "text/plain",
		FileName:         rel,
		UpdatedAt:        info.ModTime(),
		SourceResourceID: selectionRel,
		Metadata: map[string]string{
			"channel":     types.ChannelLocalDir,
			"source_type": "local_dir",
			"local_path":  rel,
			"file_size":   strconv.FormatInt(info.Size(), 10),
		},
	})
}

func deletedItem(rel string) types.FetchedItem {
	return types.FetchedItem{
		ExternalID: rel,
		Title:      rel,
		IsDeleted:  true,
		Metadata:   map[string]string{"channel": types.ChannelLocalDir},
	}
}

// --- cursor plumbing ---

type fileSig struct {
	Size  int64 `json:"size"`
	MTime int64 `json:"mtime"`
}

// cursorState is the connector cursor: the root it was built for plus a
// size+mtime signature per synced file.
type cursorState struct {
	Root  string             `json:"root"`
	Files map[string]fileSig `json:"files"`
}

func parseCursorState(cursor *types.SyncCursor) *cursorState {
	if cursor == nil || cursor.ConnectorCursor == nil {
		return nil
	}
	raw, err := json.Marshal(cursor.ConnectorCursor)
	if err != nil {
		return nil
	}
	var state cursorState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil
	}
	if state.Files == nil {
		state.Files = make(map[string]fileSig)
	}
	return &state
}

func buildSyncCursor(state *cursorState) *types.SyncCursor {
	raw, err := json.Marshal(state)
	if err != nil {
		return &types.SyncCursor{LastSyncTime: time.Now().UTC()}
	}
	var connectorCursor map[string]interface{}
	if err := json.Unmarshal(raw, &connectorCursor); err != nil {
		return &types.SyncCursor{LastSyncTime: time.Now().UTC()}
	}
	return &types.SyncCursor{
		LastSyncTime:    time.Now().UTC(),
		ConnectorCursor: connectorCursor,
	}
}

// collectHandler adapts a plain append function to the StreamHandler
// interface for the batch (FetchAll / FetchIncremental) paths. Checkpoints
// are a no-op there because the whole result is persisted at once.
type collectHandler func(types.FetchedItem) error

func (fn collectHandler) Emit(_ context.Context, item types.FetchedItem) error { return fn(item) }
func (fn collectHandler) Checkpoint(context.Context, *types.SyncCursor) error  { return nil }

// --- path helpers ---

// normalizeSelections cleans user-supplied resource IDs into deduplicated,
// non-overlapping root-relative slash paths. An empty result means "the
// whole root". Selections covered by an ancestor selection are dropped;
// paths escaping the root are rejected.
func normalizeSelections(resourceIDs []string) []string {
	if len(resourceIDs) == 0 {
		return []string{""}
	}
	cleaned := make([]string, 0, len(resourceIDs))
	seen := make(map[string]struct{}, len(resourceIDs))
	for _, id := range resourceIDs {
		rel := normalizeSlashes(id)
		if rel == "" {
			// Selecting the root covers everything.
			return []string{""}
		}
		if strings.Contains(rel, "..") {
			continue
		}
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		cleaned = append(cleaned, rel)
	}
	sort.Strings(cleaned)

	out := make([]string, 0, len(cleaned))
	for _, rel := range cleaned {
		covered := false
		for _, keep := range out {
			if rel == keep || strings.HasPrefix(rel, keep+"/") {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, rel)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func coveredByAnySelection(rel string, selections []string) bool {
	for _, sel := range selections {
		if sel == "" || rel == sel || strings.HasPrefix(rel, sel+"/") {
			return true
		}
	}
	return false
}

func underAnyPrefix(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// normalizeSlashes converts a resource ID to a clean root-relative slash
// path ("" for the root itself).
func normalizeSlashes(id string) string {
	rel := filepath.ToSlash(strings.TrimSpace(id))
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return ""
	}
	return path.Clean(rel)
}

// displayRel returns the path of p relative to root, or the basename of p
// when they are equal. Unlike filepath.Rel it never produces ".." segments.
func displayRel(root, p string) string {
	if p == root {
		return filepath.Base(p)
	}
	return strings.TrimPrefix(p, root+string(filepath.Separator))
}

func toSlashPath(p string) string { return filepath.ToSlash(p) }

func isHiddenName(name string) bool { return strings.HasPrefix(name, ".") }
