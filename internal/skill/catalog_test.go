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

func TestReadReferencePathAndContent(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "memory-audit", "---\nname: memory-audit\ndescription: Check original messages\n---\nRead references/checklist.md")
	references := filepath.Join(root, "memory-audit", "references")
	if err := os.Mkdir(references, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(references, "checklist.md"), []byte("核对原文"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	content, err := catalog.ReadReference("memory-audit", "references/checklist.md")
	if err != nil || content != "核对原文" {
		t.Fatalf("unexpected reference=%q err=%v", content, err)
	}
	for _, path := range []string{"SKILL.md", "../other", "references/../SKILL.md", "references/./checklist.md", "references/missing.md"} {
		if _, err := catalog.ReadReference("memory-audit", path); err == nil {
			t.Errorf("expected path %q to be rejected", path)
		}
	}
	if _, err := catalog.ReadReference("unknown", "references/checklist.md"); err == nil {
		t.Fatal("unknown skill must be rejected")
	}
}

func TestReadReferenceRejectsInvalidFiles(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "memory-audit", "---\nname: memory-audit\ndescription: Check original messages\n---\nbody")
	references := filepath.Join(root, "memory-audit", "references")
	if err := os.Mkdir(references, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"invalid.md": {0xff},
		"large.md":   []byte(strings.Repeat("x", maxReferenceFileBytes+1)),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(references, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for name := range files {
		if _, err := catalog.ReadReference("memory-audit", "references/"+name); err == nil {
			t.Errorf("expected %q to be rejected", name)
		}
	}
	if err := os.Symlink(filepath.Join(root, "memory-audit", "SKILL.md"), filepath.Join(references, "linked.md")); err == nil {
		if _, err := catalog.ReadReference("memory-audit", "references/linked.md"); err == nil {
			t.Fatal("symlink must be rejected")
		}
	}
}
