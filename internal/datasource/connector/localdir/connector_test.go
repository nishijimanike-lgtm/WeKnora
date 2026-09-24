package localdir

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// recordingHandler captures emitted items and checkpoints.
type recordingHandler struct {
	items       []types.FetchedItem
	checkpoints int
	emitErr     error
}

func (h *recordingHandler) Emit(_ context.Context, item types.FetchedItem) error {
	if h.emitErr != nil {
		return h.emitErr
	}
	h.items = append(h.items, item)
	return nil
}

func (h *recordingHandler) Checkpoint(context.Context, *types.SyncCursor) error {
	h.checkpoints++
	return nil
}

// setupRoot creates an allowlisted temp root and points the connector at it.
func setupRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(AllowedRootsEnvVar, root)
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func configFor(root string, settings map[string]interface{}) *types.DataSourceConfig {
	if settings == nil {
		settings = map[string]interface{}{}
	}
	settings["root_path"] = root
	return &types.DataSourceConfig{
		Type:     types.ConnectorTypeLocalDir,
		Settings: settings,
	}
}

func TestValidate(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()

	t.Run("disabled without allowlist", func(t *testing.T) {
		t.Setenv(AllowedRootsEnvVar, "")
		err := c.Validate(ctx, configFor(t.TempDir(), nil))
		if err == nil || !strings.Contains(err.Error(), AllowedRootsEnvVar) {
			t.Fatalf("expected disabled error, got %v", err)
		}
	})

	t.Run("root outside allowlist", func(t *testing.T) {
		allowed := t.TempDir()
		t.Setenv(AllowedRootsEnvVar, allowed)
		err := c.Validate(ctx, configFor(t.TempDir(), nil))
		if err == nil || !strings.Contains(err.Error(), "not under any allowed directory") {
			t.Fatalf("expected allowlist error, got %v", err)
		}
	})

	t.Run("allowlist escape via ..", func(t *testing.T) {
		allowed := t.TempDir()
		t.Setenv(AllowedRootsEnvVar, allowed)
		err := c.Validate(ctx, configFor(filepath.Join(allowed, "..", ".."), nil))
		if err == nil {
			t.Fatal("expected traversal to be rejected")
		}
	})

	t.Run("missing root", func(t *testing.T) {
		root := setupRoot(t)
		err := c.Validate(ctx, configFor(filepath.Join(root, "nope"), nil))
		if err == nil {
			t.Fatal("expected error for missing root")
		}
	})

	t.Run("root is a file", func(t *testing.T) {
		root := setupRoot(t)
		file := filepath.Join(root, "file.txt")
		writeFile(t, file, "x")
		if err := c.Validate(ctx, configFor(file, nil)); err == nil {
			t.Fatal("expected error when root is a file")
		}
	})

	t.Run("valid root", func(t *testing.T) {
		root := setupRoot(t)
		if err := c.Validate(ctx, configFor(root, nil)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("root_path falls back to credentials", func(t *testing.T) {
		root := setupRoot(t)
		cfg := &types.DataSourceConfig{
			Type:        types.ConnectorTypeLocalDir,
			Credentials: map[string]interface{}{"root_path": root},
		}
		if err := c.Validate(ctx, cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestParseConfigCoercion(t *testing.T) {
	cfg, err := parseConfig(&types.DataSourceConfig{
		Type: types.ConnectorTypeLocalDir,
		Settings: map[string]interface{}{
			"root_path":        " /data/docs ",
			"include_hidden":   "true",
			"max_file_size_mb": "42",
			"file_extensions":  "PDF, md ,.txt",
		},
	})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.RootPath != "/data/docs" {
		t.Errorf("root path = %q", cfg.RootPath)
	}
	if !cfg.IncludeHidden {
		t.Error("include_hidden not coerced to true")
	}
	if cfg.MaxFileSizeMB != 42 {
		t.Errorf("max_file_size_mb = %d", cfg.MaxFileSizeMB)
	}
	if cfg.maxFileSizeBytes() != 42*1024*1024 {
		t.Errorf("maxFileSizeBytes = %d", cfg.maxFileSizeBytes())
	}
	if !cfg.isSupportedFile("A.PDF") || !cfg.isSupportedFile("b.md") || !cfg.isSupportedFile("c.txt") {
		t.Error("extension filter did not match configured extensions")
	}
	if cfg.isSupportedFile("d.docx") {
		t.Error("docx should be excluded when an explicit filter is set")
	}

	if _, err := parseConfig(&types.DataSourceConfig{Type: types.ConnectorTypeLocalDir}); err == nil {
		t.Error("expected error for missing root_path")
	}
}

func TestNormalizeSelections(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, []string{""}},
		{[]string{}, []string{""}},
		{[]string{""}, []string{""}},
		{[]string{"a", "a/b", "a/b/c"}, []string{"a"}},
		{[]string{"b", "a"}, []string{"a", "b"}},
		{[]string{"/a/", "a"}, []string{"a"}},
		{[]string{"../etc", "a"}, []string{"a"}},
		{[]string{"../../x"}, []string{""}},
	}
	for _, tc := range cases {
		got := normalizeSelections(tc.in)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("normalizeSelections(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestListResources(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()
	root := setupRoot(t)

	writeFile(t, filepath.Join(root, "reports", "2024", "q1.pdf"), "pdf")
	writeFile(t, filepath.Join(root, "reports", "notes.md"), "md")
	writeFile(t, filepath.Join(root, "contracts", "a.docx"), "docx")
	writeFile(t, filepath.Join(root, "top.txt"), "top")
	writeFile(t, filepath.Join(root, ".hidden", "secret.txt"), "hidden")
	writeFile(t, filepath.Join(root, "binary.bin"), "nope")
	if err := os.Symlink(filepath.Join(root, "contracts"), filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res, err := c.ListResources(ctx, configFor(root, nil), "")
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	var names []string
	for _, r := range res {
		names = append(names, r.Type+":"+r.ExternalID)
	}
	want := []string{"directory:contracts", "directory:reports", "file:top.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("root listing = %v, want %v", names, want)
	}
	if !res[0].HasChildren || !res[1].HasChildren {
		t.Error("directories must be marked has_children")
	}

	// Second level, lazy.
	res, err = c.ListResources(ctx, configFor(root, nil), "reports")
	if err != nil {
		t.Fatalf("ListResources(reports): %v", err)
	}
	names = nil
	for _, r := range res {
		names = append(names, r.Type+":"+r.ExternalID+":"+r.ParentID)
	}
	want = []string{"directory:reports/2024:reports", "file:reports/notes.md:reports"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("reports listing = %v, want %v", names, want)
	}

	// Hidden entries become visible with include_hidden.
	res, err = c.ListResources(ctx, configFor(root, map[string]interface{}{"include_hidden": true}), "")
	if err != nil {
		t.Fatalf("ListResources(include_hidden): %v", err)
	}
	foundHidden := false
	for _, r := range res {
		if r.ExternalID == ".hidden" {
			foundHidden = true
		}
	}
	if !foundHidden {
		t.Error("include_hidden should list dot-directories")
	}

	// Traversal attempts are rejected.
	if _, err := c.ListResources(ctx, configFor(root, nil), "../../etc"); err == nil {
		t.Error("expected traversal rejection")
	}
}

func TestResolveResourceAncestors(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()

	ancestors, err := c.ResolveResourceAncestors(ctx, nil, []string{"a/b/c.md", "a/d", "top.txt", ""})
	if err != nil {
		t.Fatalf("ResolveResourceAncestors: %v", err)
	}
	sort.Strings(ancestors)
	want := []string{"a", "a/b"}
	if strings.Join(ancestors, ",") != strings.Join(want, ",") {
		t.Fatalf("ancestors = %v, want %v", ancestors, want)
	}
}

func TestFetchStreamFullSync(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()
	root := setupRoot(t)

	writeFile(t, filepath.Join(root, "reports", "q1.md"), "hello")
	writeFile(t, filepath.Join(root, "reports", "deep", "q2.md"), "world")
	writeFile(t, filepath.Join(root, "notes.txt"), "top")
	writeFile(t, filepath.Join(root, "skip.bin"), "binary")

	h := &recordingHandler{}
	cursor, err := c.FetchStream(ctx, configFor(root, nil), nil, h)
	if err != nil {
		t.Fatalf("FetchStream: %v", err)
	}
	if len(h.items) != 3 {
		t.Fatalf("items = %d, want 3", len(h.items))
	}
	byID := map[string]types.FetchedItem{}
	for _, item := range h.items {
		byID[item.ExternalID] = item
	}
	item, ok := byID["reports/deep/q2.md"]
	if !ok {
		t.Fatalf("missing reports/deep/q2.md in %v", keys(byID))
	}
	if item.FileName != "reports/deep/q2.md" {
		t.Errorf("FileName = %q, want root-relative path", item.FileName)
	}
	if item.Metadata["channel"] != types.ChannelLocalDir {
		t.Errorf("channel = %q", item.Metadata["channel"])
	}
	if item.SourceResourceID != "" {
		t.Errorf("SourceResourceID = %q, want root selection", item.SourceResourceID)
	}
	if cursor == nil || cursor.ConnectorCursor == nil {
		t.Fatal("expected a cursor")
	}

	// Scoped selection: only the reports subtree, attributed to the selection.
	h = &recordingHandler{}
	cfg := configFor(root, nil)
	cfg.ResourceIDs = []string{"reports"}
	if _, err := c.FetchStream(ctx, cfg, nil, h); err != nil {
		t.Fatalf("FetchStream(reports): %v", err)
	}
	if len(h.items) != 2 {
		t.Fatalf("items = %d, want 2", len(h.items))
	}
	for _, item := range h.items {
		if item.SourceResourceID != "reports" {
			t.Errorf("SourceResourceID = %q, want reports", item.SourceResourceID)
		}
	}
}

func TestFetchStreamIncremental(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()
	root := setupRoot(t)

	report := filepath.Join(root, "reports", "q1.md")
	stable := filepath.Join(root, "stable.txt")
	writeFile(t, report, "v1")
	writeFile(t, stable, "stable")
	writeFile(t, filepath.Join(root, "gone.md"), "to be removed")

	cfg := configFor(root, nil)

	h := &recordingHandler{}
	cursor, err := c.FetchStream(ctx, cfg, nil, h)
	if err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if len(h.items) != 3 {
		t.Fatalf("initial items = %d, want 3", len(h.items))
	}

	// No changes → nothing emitted.
	h = &recordingHandler{}
	cursor, err = c.FetchStream(ctx, cfg, cursor, h)
	if err != nil {
		t.Fatalf("no-op sync: %v", err)
	}
	if len(h.items) != 0 {
		t.Fatalf("no-op items = %d, want 0: %v", len(h.items), itemIDs(h.items))
	}

	// Modify one, add one, delete one.
	writeFile(t, report, "v2-changed")
	// Bump mtime explicitly: some filesystems have second-granularity mtimes.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(report, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	writeFile(t, filepath.Join(root, "fresh.md"), "fresh")
	if err := os.Remove(filepath.Join(root, "gone.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	h = &recordingHandler{}
	cursor, err = c.FetchStream(ctx, cfg, cursor, h)
	if err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	var updated, created, deleted []string
	for _, item := range h.items {
		switch {
		case item.IsDeleted:
			deleted = append(deleted, item.ExternalID)
		case item.ExternalID == "reports/q1.md":
			updated = append(updated, item.ExternalID)
			if string(item.Content) != "v2-changed" {
				t.Errorf("updated content = %q", item.Content)
			}
		default:
			created = append(created, item.ExternalID)
		}
	}
	if strings.Join(updated, ",") != "reports/q1.md" {
		t.Errorf("updated = %v", updated)
	}
	if strings.Join(created, ",") != "fresh.md" {
		t.Errorf("created = %v", created)
	}
	if strings.Join(deleted, ",") != "gone.md" {
		t.Errorf("deleted = %v", deleted)
	}

	// Deleting a whole selected directory deletes its files.
	cfgDir := configFor(root, nil)
	cfgDir.ResourceIDs = []string{"reports"}
	h = &recordingHandler{}
	dirCursor, err := c.FetchStream(ctx, cfgDir, nil, h)
	if err != nil {
		t.Fatalf("dir sync: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "reports")); err != nil {
		t.Fatalf("remove reports: %v", err)
	}
	h = &recordingHandler{}
	if _, err := c.FetchStream(ctx, cfgDir, dirCursor, h); err != nil {
		t.Fatalf("dir deletion sync: %v", err)
	}
	if len(h.items) != 1 || !h.items[0].IsDeleted || h.items[0].ExternalID != "reports/q1.md" {
		t.Fatalf("expected deletion of reports/q1.md, got %v", itemIDs(h.items))
	}
}

func TestFetchStreamScopeNarrowingKeepsKnowledge(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()
	root := setupRoot(t)

	writeFile(t, filepath.Join(root, "a", "one.md"), "one")
	writeFile(t, filepath.Join(root, "b", "two.md"), "two")

	all := configFor(root, nil)
	h := &recordingHandler{}
	cursor, err := c.FetchStream(ctx, all, nil, h)
	if err != nil {
		t.Fatalf("initial: %v", err)
	}

	// Narrow the scope to folder "a": "b/two.md" must be carried over in
	// the cursor, NOT emitted as a deletion.
	narrow := configFor(root, nil)
	narrow.ResourceIDs = []string{"a"}
	h = &recordingHandler{}
	cursor, err = c.FetchStream(ctx, narrow, cursor, h)
	if err != nil {
		t.Fatalf("narrow: %v", err)
	}
	for _, item := range h.items {
		if item.IsDeleted {
			t.Fatalf("scope narrowing must not delete, got %v", itemIDs(h.items))
		}
	}
	state := parseCursorState(cursor)
	if state == nil {
		t.Fatal("cursor lost")
	}
	if _, ok := state.Files["b/two.md"]; !ok {
		t.Errorf("out-of-scope entry must be carried over, cursor files = %v", keys2(state.Files))
	}
}

func TestFetchStreamRootChangeForcesFull(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()

	rootA := t.TempDir()
	rootB := t.TempDir()
	t.Setenv(AllowedRootsEnvVar, rootA+","+rootB)

	writeFile(t, filepath.Join(rootA, "same.md"), "content-A")
	writeFile(t, filepath.Join(rootB, "same.md"), "content-B")

	h := &recordingHandler{}
	cursor, err := c.FetchStream(ctx, configFor(rootA, nil), nil, h)
	if err != nil {
		t.Fatalf("sync A: %v", err)
	}

	h = &recordingHandler{}
	if _, err := c.FetchStream(ctx, configFor(rootB, nil), cursor, h); err != nil {
		t.Fatalf("sync B: %v", err)
	}
	if len(h.items) != 1 || h.items[0].IsDeleted || string(h.items[0].Content) != "content-B" {
		t.Fatalf("expected full re-ingest after root change, got %v", h.items)
	}
}

func TestFetchStreamSkipsIneligibleWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()
	root := setupRoot(t)

	big := filepath.Join(root, "big.pdf")
	writeFile(t, big, "small-at-first")
	writeFile(t, filepath.Join(root, "keep.md"), "keep")

	cfg := configFor(root, nil)
	h := &recordingHandler{}
	cursor, err := c.FetchStream(ctx, cfg, nil, h)
	if err != nil {
		t.Fatalf("initial: %v", err)
	}

	// Grow the file beyond the cap (use a 1 MB cap to keep the test cheap).
	cfg = configFor(root, map[string]interface{}{"max_file_size_mb": 1})
	if f, err := os.OpenFile(big, os.O_WRONLY|os.O_APPEND, 0); err == nil {
		chunk := make([]byte, 1024*1024)
		if _, err := f.Write(chunk); err != nil {
			t.Fatalf("grow: %v", err)
		}
		_ = f.Close()
	}

	h = &recordingHandler{}
	cursor, err = c.FetchStream(ctx, cfg, cursor, h)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	for _, item := range h.items {
		if item.ExternalID == "big.pdf" {
			t.Fatalf("oversized file must be skipped, got %+v", item)
		}
	}
	state := parseCursorState(cursor)
	if _, ok := state.Files["big.pdf"]; !ok {
		t.Error("previous signature of an oversized file must be carried over (no deletion)")
	}
}

func TestFetchStreamRejectsEscapingSelection(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()
	root := setupRoot(t)
	writeFile(t, filepath.Join(root, "keep.md"), "keep")

	cfg := configFor(root, nil)
	cfg.ResourceIDs = []string{"../../etc"}
	h := &recordingHandler{}
	_, err := c.FetchStream(ctx, cfg, nil, h)
	// The escaping selection is dropped; remaining selections fall back to
	// the whole root, so the sync succeeds but must not read outside root.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, item := range h.items {
		if strings.Contains(item.ExternalID, "..") {
			t.Fatalf("escaping path leaked into items: %v", item.ExternalID)
		}
	}
}

func TestFetchAllAndIncrementalCompat(t *testing.T) {
	ctx := context.Background()
	c := NewConnector()
	root := setupRoot(t)
	writeFile(t, filepath.Join(root, "docs", "a.md"), "a")

	items, err := c.FetchAll(ctx, configFor(root, nil), nil)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(items) != 1 || items[0].ExternalID != "docs/a.md" {
		t.Fatalf("FetchAll items = %+v", items)
	}

	cfg := configFor(root, nil)
	items2, cursor, err := c.FetchIncremental(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("FetchIncremental(full): %v", err)
	}
	if len(items2) != 1 {
		t.Fatalf("FetchIncremental items = %d", len(items2))
	}
	items3, cursor2, err := c.FetchIncremental(ctx, cfg, cursor)
	if err != nil {
		t.Fatalf("FetchIncremental(noop): %v", err)
	}
	if len(items3) != 0 {
		t.Fatalf("expected no-op incremental, got %v", itemIDs(items3))
	}
	if cursor2 == nil {
		t.Fatal("expected cursor")
	}
}

// --- helpers ---

func keys(m map[string]types.FetchedItem) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keys2(m map[string]fileSig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func itemIDs(items []types.FetchedItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		prefix := ""
		if item.IsDeleted {
			prefix = "deleted:"
		}
		out = append(out, prefix+item.ExternalID)
	}
	sort.Strings(out)
	return out
}
