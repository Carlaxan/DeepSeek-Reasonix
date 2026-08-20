package sessioncatalog

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openMetadataTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "catalog.sqlite"), DisableRepair: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close(context.Background()) })
	return catalog
}

func metadataPresentFlag(t *testing.T, catalog *Catalog, topicID string) (int, bool) {
	t.Helper()
	var present int
	err := catalog.db.QueryRowContext(context.Background(),
		`SELECT metadata_present FROM catalog_topics WHERE scope='global' AND workspace_root='' AND topic_id=?`, topicID).
		Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return present, true
}

// TestSyncMetadataSkipsFoldedRecoveryShell covers the resurrection guard: a
// registry topic whose sessions are all recovery copies of a lineage that
// already has a canonical/ordinary representative must not be (re)created as a
// metadata shell. See #8525/#8551.
func TestSyncMetadataSkipsFoldedRecoveryShell(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name      string
		canonical SessionRecord
		pinned    bool
		wantSkip  bool
	}{
		{
			name: "flagged canonical member",
			canonical: SessionRecord{
				Path: "/s/leaf.jsonl", Directory: "/s", Scope: "global", TopicID: "canon",
				Recovered: true, ParentID: "root", OrdinaryVisible: true,
				Turns: 2, TurnsState: TurnsValid, Health: HealthOK,
			},
			wantSkip: true,
		},
		{
			name: "ordinary group root",
			canonical: SessionRecord{
				Path: "/s/root.jsonl", Directory: "/s", Scope: "global", TopicID: "canon",
				Turns: 3, TurnsState: TurnsValid, Health: HealthOK,
			},
			wantSkip: true,
		},
		{
			name:     "unresolved lineage keeps shell",
			wantSkip: false,
		},
		{
			name: "pinned shell preserved",
			canonical: SessionRecord{
				Path: "/s/root.jsonl", Directory: "/s", Scope: "global", TopicID: "canon",
				Turns: 3, TurnsState: TurnsValid, Health: HealthOK,
			},
			pinned:   true,
			wantSkip: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			catalog := openMetadataTestCatalog(t)
			copy := SessionRecord{
				Path: "/s/copy.jsonl", Directory: "/s", Scope: "global", TopicID: "copy-topic",
				Recovered: true, RecoveryCopy: true, ParentID: "root",
				Turns: 1, TurnsState: TurnsValid, Health: HealthOK,
			}
			if err := catalog.UpsertSession(ctx, copy); err != nil {
				t.Fatal(err)
			}
			if tc.canonical.Path != "" {
				if err := catalog.UpsertSession(ctx, tc.canonical); err != nil {
					t.Fatal(err)
				}
			}
			if err := catalog.SyncMetadata(ctx, nil, []TopicMetadata{
				{Scope: "global", TopicID: "copy-topic", Title: "Recovered unsaved changes from stale runtime", Pinned: tc.pinned},
			}); err != nil {
				t.Fatal(err)
			}
			present, exists := metadataPresentFlag(t, catalog, "copy-topic")
			if !exists {
				t.Fatal("copy-topic row missing: the session-derived projection must stay")
			}
			if tc.wantSkip && present != 0 {
				t.Fatal("folded recovery shell resurrected with metadata_present=1")
			}
			if !tc.wantSkip && present != 1 {
				t.Fatal("recovery shell without canonical must keep metadata continuity")
			}
		})
	}
}

// TestSyncMetadataDoesNotResurrectReanchoredRecoveryTopic runs the full
// resurrection sequence: shell skip, lineage re-anchor (copy moves onto the
// canonical topic, empty shell deleted), then a later metadata sync that must
// not bring the copy's topic back.
func TestSyncMetadataDoesNotResurrectReanchoredRecoveryTopic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	catalog := openMetadataTestCatalog(t)
	copy := SessionRecord{
		Path: "/s/copy.jsonl", Directory: "/s", Scope: "global", TopicID: "copy-topic",
		Recovered: true, RecoveryCopy: true, ParentID: "root",
		Turns: 1, TurnsState: TurnsValid, Health: HealthOK,
	}
	root := SessionRecord{
		Path: "/s/root.jsonl", Directory: "/s", Scope: "global", TopicID: "canon",
		Turns: 3, TurnsState: TurnsValid, Health: HealthOK,
	}
	for _, record := range []SessionRecord{copy, root} {
		if err := catalog.UpsertSession(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	metadata := []TopicMetadata{{Scope: "global", TopicID: "copy-topic", Title: "Recovered"}}
	if err := catalog.SyncMetadata(ctx, nil, metadata); err != nil {
		t.Fatal(err)
	}
	if present, _ := metadataPresentFlag(t, catalog, "copy-topic"); present != 0 {
		t.Fatal("folded shell should have been skipped before the re-anchor")
	}
	// Lineage projection re-anchors the copy onto the canonical topic; the empty
	// pre-reanchor topic is deleted by the topic recompute.
	copy.TopicID = "canon"
	if err := catalog.UpsertSession(ctx, copy); err != nil {
		t.Fatal(err)
	}
	if _, exists := metadataPresentFlag(t, catalog, "copy-topic"); exists {
		t.Fatal("re-anchored copy topic should have been deleted")
	}
	if err := catalog.SyncMetadata(ctx, nil, metadata); err != nil {
		t.Fatal(err)
	}
	if _, exists := metadataPresentFlag(t, catalog, "copy-topic"); exists {
		t.Fatal("metadata sync resurrected a folded recovery topic")
	}
}

// TestSyncMetadataKeepsMixedTopicWithOrdinarySession ensures the guard never
// hides a topic that still has an ordinary (non-recovery) session.
func TestSyncMetadataKeepsMixedTopicWithOrdinarySession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	catalog := openMetadataTestCatalog(t)
	for _, record := range []SessionRecord{
		{Path: "/s/normal.jsonl", Directory: "/s", Scope: "global", TopicID: "mixed",
			Turns: 2, TurnsState: TurnsValid, Health: HealthOK},
		{Path: "/s/copy.jsonl", Directory: "/s", Scope: "global", TopicID: "mixed",
			Recovered: true, RecoveryCopy: true, ParentID: "root",
			Turns: 1, TurnsState: TurnsValid, Health: HealthOK},
		{Path: "/s/root.jsonl", Directory: "/s", Scope: "global", TopicID: "canon",
			Turns: 3, TurnsState: TurnsValid, Health: HealthOK},
	} {
		if err := catalog.UpsertSession(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.SyncMetadata(ctx, nil, []TopicMetadata{
		{Scope: "global", TopicID: "mixed", Title: "Mixed topic"},
	}); err != nil {
		t.Fatal(err)
	}
	present, exists := metadataPresentFlag(t, catalog, "mixed")
	if !exists || present != 1 {
		t.Fatalf("mixed topic metadata lost: exists=%v present=%d", exists, present)
	}
}
