package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, directory, contents string) {
	t.Helper()
	path := filepath.Join(root, directory)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogLoadsMetadataBeforeBody(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "memory-audit", "---\r\nname: memory-audit\r\ndescription: Check original messages\r\n---\r\n\r\nFollow the source.\r\n")
	catalog, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	items := catalog.List()
	if len(items) != 1 || items[0].Name != "memory-audit" || items[0].Description != "Check original messages" {
		t.Fatalf("unexpected catalog: %+v", items)
	}
	body, err := catalog.Load("memory-audit")
	if err != nil || body != "Follow the source." {
		t.Fatalf("unexpected body=%q err=%v", body, err)
	}
	if _, err := catalog.Load("../other"); err == nil {
		t.Fatal("unknown skill path must be rejected")
	}
}

func TestCatalogRejectsInvalidSkills(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
	}{
		{"mismatched", "---\nname: another-name\ndescription: test\n---\nbody"},
		{"invalid-name", "---\nname: invalid--name\ndescription: test\n---\nbody"},
		{"missing-description", "---\nname: missing-description\n---\nbody"},
		{"missing-frontmatter", "plain Markdown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeSkill(t, root, test.name, test.text)
			if _, err := Open(root); err == nil {
				t.Fatal("expected invalid skill to fail")
			}
		})
	}
}

func TestMissingSkillDirectoryIsOptional(t *testing.T) {
	catalog, err := Open(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(catalog.List()) != 0 {
		t.Fatalf("expected empty optional catalog, got %+v err=%v", catalog, err)
	}
}

func TestSkillFileSizeLimit(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "large", "---\nname: large\ndescription: test\n---\n"+strings.Repeat("x", maxSkillFileBytes))
	if _, err := Open(root); err == nil {
		t.Fatal("oversized skill must be rejected")
	}
}
