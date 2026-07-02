package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cgvant/internal/config"
)

// newTestEditor builds an editModel over a fresh temp CGVANT_HOME with an empty
// snapshot "s". It drives the model's data methods directly (no TTY/rendering).
func newTestEditor(t *testing.T) *editModel {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CGVANT_HOME", dir)
	cfgPath := filepath.Join(dir, "config.toml")
	body := "data_dir = \"data\"\nannotations_dir = \".\"\ndefault_snapshot = \"s\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// An empty snapshot manifest the editor's saveSource will add refs to.
	if err := config.WriteSnapshotConfig(cfg.SnapshotFile("s"), &config.SnapshotConfig{Assembly: "GRCh38"}); err != nil {
		t.Fatal(err)
	}
	return &editModel{cfg: cfg, cfgPath: cfgPath, width: 80, height: 24, curSnap: "s"}
}

func TestEditorSaveAndReloadSource(t *testing.T) {
	m := newTestEditor(t)

	// New source with a nested annotation → save → reload.
	m.startNewSource()
	m.curSource.Name = "clinvar"
	m.curSource.Version = "2026-01"
	m.curSource.Format = "vcf"
	m.curSource.URL = "https://x/clinvar.vcf.gz"
	m.curSource.Annotations = append(m.curSource.Annotations,
		config.Annotation{Name: "clinvar_sig", Field: "CLNSIG", Type: "categorical"})
	if err := m.saveSource(); err != nil {
		t.Fatalf("saveSource: %v", err)
	}

	snap, err := m.cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(snap.Sources) != 1 || snap.Sources[0].ID() != "clinvar:2026-01" {
		t.Fatalf("sources = %+v", snap.Sources)
	}
	if len(snap.Annotations) != 1 || snap.Annotations[0].Name != "clinvar_sig" || snap.Annotations[0].Source != "clinvar" {
		t.Fatalf("annotations = %+v", snap.Annotations)
	}

	// Re-open the written fragment and confirm the working source round-trips.
	m.openSource(m.curPath)
	if m.curSource == nil || m.curSource.Name != "clinvar" {
		t.Fatalf("openSource = %+v", m.curSource)
	}
}

func TestEditorSaveSourceMissingFields(t *testing.T) {
	m := newTestEditor(t)
	m.startNewSource() // no name/version/url
	if err := m.saveSource(); err == nil {
		t.Fatal("expected a missing-required-field error")
	}
}

func TestEditorTabColsClearedForVCF(t *testing.T) {
	m := newTestEditor(t)
	m.startNewSource()
	m.curSource.Name, m.curSource.Version, m.curSource.URL = "x", "1", "https://x"
	m.curSource.Format = "vcf"
	m.refColStr, m.altColStr = "3", "4" // set, but format is vcf → must be dropped
	if err := m.saveSource(); err != nil {
		t.Fatal(err)
	}
	snap, _ := m.cfg.LoadSnapshot("s")
	if snap.Sources[0].RefCol != 0 || snap.Sources[0].AltCol != 0 {
		t.Errorf("vcf source kept ref/alt cols: %+v", snap.Sources[0])
	}
}

func TestEditorBuiltinsAddAndRemove(t *testing.T) {
	m := newTestEditor(t)
	m.toBuiltins("s") // find-or-create the builtin fragment
	m.appendBuiltin("tstv", "")
	m.appendBuiltin("tags", "PANEL:v1")
	if err := m.saveBuiltins(); err != nil {
		t.Fatal(err)
	}
	snap, err := m.cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := map[string]string{}
	for _, a := range snap.Annotations {
		got[a.Source] = a.Args // builtin source == builtin name
	}
	if _, ok := got["tstv"]; !ok {
		t.Errorf("tstv missing: %+v", snap.Annotations)
	}
	if got["tags"] != "PANEL:v1" {
		t.Errorf("tags args = %q, want PANEL:v1", got["tags"])
	}

	// Remove tstv, keep tags.
	m.removeBuiltin("tstv")
	if err := m.saveBuiltins(); err != nil {
		t.Fatal(err)
	}
	snap, _ = m.cfg.LoadSnapshot("s")
	if len(snap.Annotations) != 1 || snap.Annotations[0].Source != "tags" {
		t.Errorf("after remove = %+v", snap.Annotations)
	}
}

// TestEditorSnapshotMembersAndDefaults drives the manifest-level checkbox editors:
// membership rewrites the snapshot's sources list, defaults writes
// default_annotations, and dropping a source prunes now-orphaned defaults.
func TestEditorSnapshotDefaults(t *testing.T) {
	m := newTestEditor(t)

	// Add a source with one annotation; saveSource references it from the snapshot.
	m.startNewSource()
	m.curSource.Name, m.curSource.Version, m.curSource.Format = "clinvar", "2026-01", "vcf"
	m.curSource.URL = "https://x/clinvar.vcf.gz"
	m.curSource.Annotations = append(m.curSource.Annotations,
		config.Annotation{Name: "clinvar_sig", Field: "CLNSIG", Type: "categorical"})
	if err := m.saveSource(); err != nil {
		t.Fatalf("saveSource: %v", err)
	}

	// The defaults editor should offer exactly that annotation.
	if names := m.snapshotAnnotationNames(); len(names) != 1 || names[0] != "clinvar_sig" {
		t.Fatalf("annotation options = %v, want [clinvar_sig]", names)
	}

	// Mark it default and persist to the manifest.
	m.defaultAnns = []string{"clinvar_sig"}
	if err := m.saveSnapDefaults(); err != nil {
		t.Fatalf("saveSnapDefaults: %v", err)
	}
	sc, err := config.ReadSnapshotConfig(m.cfg.SnapshotFile("s"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Defaults) != 1 || sc.Defaults[0] != "clinvar_sig" {
		t.Fatalf("manifest defaults = %v, want [clinvar_sig]", sc.Defaults)
	}
	// LoadSnapshot derives Annotation.Default from the manifest.
	snap, err := m.cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Annotations) != 1 || !snap.Annotations[0].Default {
		t.Fatalf("expected clinvar_sig marked default, got %+v", snap.Annotations)
	}

	// Removing the source via the members editor prunes the orphaned default.
	m.memberSources = nil
	if err := m.saveSnapMembers(); err != nil {
		t.Fatalf("saveSnapMembers: %v", err)
	}
	sc, _ = config.ReadSnapshotConfig(m.cfg.SnapshotFile("s"))
	if len(sc.Sources) != 0 {
		t.Errorf("sources not cleared: %v", sc.Sources)
	}
	if len(sc.Defaults) != 0 {
		t.Errorf("orphaned default not pruned: %v", sc.Defaults)
	}
}

func TestEditorViewRenders(t *testing.T) {
	m := newTestEditor(t)
	m.width, m.height = 90, 24
	m.toSnapshots()
	out := m.View()
	for _, want := range []string{"cgvant", "snapshots", "quit"} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshots view missing %q", want)
		}
	}
	// A source form renders without panic, showing the breadcrumb + first field.
	m.startNewSource()
	m.toSourceForm()
	out = m.View()
	for _, want := range []string{"cgvant", "source", "name"} {
		if !strings.Contains(out, want) {
			t.Errorf("source form missing %q", want)
		}
	}
}
