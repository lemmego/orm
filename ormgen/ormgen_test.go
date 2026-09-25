package ormgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "../internal/genfixture"

// The committed descriptors must match what the generator produces now. A
// failure here means the fixture models changed without regenerating, or the
// generator's output changed unintentionally.
func TestGeneratedFixtureIsUpToDate(t *testing.T) {
	generated, err := Generate(Options{Dir: fixtureDir})
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(filepath.Join(fixtureDir, OutputFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(generated) != string(committed) {
		t.Fatalf("%s is stale; regenerate with:\n  go run ./cmd/ormgen %s", OutputFileName, fixtureDir)
	}
}

func TestGeneratorSkipsUntaggedStructsByDefault(t *testing.T) {
	generated, err := Generate(Options{Dir: fixtureDir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(generated), "NotAModelFields") {
		t.Fatal("a struct with no mapping tags must be skipped by default")
	}
	if strings.Contains(string(generated), "TimestampsFields") {
		t.Fatal("an embedded-only struct with no tags must be skipped by default")
	}
}

func TestGeneratorAllIncludesUntaggedStructs(t *testing.T) {
	generated, err := Generate(Options{Dir: fixtureDir, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), "NotAModelFields") {
		t.Fatal("-all should include untagged structs")
	}
}

func TestGeneratorTypeFilter(t *testing.T) {
	generated, err := Generate(Options{Dir: fixtureDir, Only: []string{"Post"}})
	if err != nil {
		t.Fatal(err)
	}
	source := string(generated)
	if !strings.Contains(source, "PostFields") {
		t.Fatal("expected the selected type to be generated")
	}
	if strings.Contains(source, "UserFields") {
		t.Fatal("expected unselected types to be omitted")
	}
}

// The generator must read a literal TableName() override rather than assuming
// the conventional name.
func TestGeneratorHonoursTableNameOverride(t *testing.T) {
	generated, err := Generate(Options{Dir: fixtureDir, Only: []string{"Account"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), `const AccountTable = "billing_accounts"`) {
		t.Fatalf("expected the TableName() override to be used:\n%s", generated)
	}
}

func TestGeneratorReportsEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte("package empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(Options{Dir: dir}); err == nil {
		t.Fatal("expected an error when no models are found")
	}
}
