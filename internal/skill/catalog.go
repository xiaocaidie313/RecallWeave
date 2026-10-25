package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const maxSkillFileBytes = 128 << 10
const maxReferenceFileBytes = 64 << 10

var validName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type Metadata struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
}

type Catalog struct {
	root   string
	skills map[string]Metadata
}

// Open reads skill metadata from direct child directories. A missing directory
// means that skills are optional, not that server startup should fail.
func Open(root string) (*Catalog, error) {
	catalog := &Catalog{root: root, skills: make(map[string]Metadata)}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return catalog, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		// 是否是文件夹
		if !entry.IsDir() {
			continue
		}
		// 读取SKILL.md文件
		path := filepath.Join(root, entry.Name(), "SKILL.md")
		metadata, _, err := readSkill(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", entry.Name(), err)
		}
		if metadata.Name != entry.Name() {
			return nil, fmt.Errorf("skill %q: name must match directory", entry.Name())
		}
		catalog.skills[metadata.Name] = metadata
	}
	return catalog, nil
}

func (c *Catalog) List() []Metadata {
	items := make([]Metadata, 0, len(c.skills))
	for _, item := range c.skills {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}

// Load returns instructions only for a discovered skill. Names are looked up
// in the catalog, not used directly as paths supplied by an agent.
func (c *Catalog) Load(name string) (string, error) {
	if _, ok := c.skills[name]; !ok {
		return "", fmt.Errorf("unknown skill %q", name)
	}
	metadata, body, err := readSkill(filepath.Join(c.root, name, "SKILL.md"))
	if err != nil {
		return "", err
	}
	if metadata.Name != name {
		return "", fmt.Errorf("skill %q changed its name", name)
	}
	return body, nil
}

// ReadReference reads one text resource from a discovered Skill's references
// directory. It never accepts an absolute path, traversal, or symlink.
func (c *Catalog) ReadReference(name, path string) (string, error) {
	if _, ok := c.skills[name]; !ok {
		return "", fmt.Errorf("unknown skill %q", name)
	}
	path = filepath.FromSlash(path)
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("invalid reference path %q", path)
	}
	parts := strings.Split(path, string(filepath.Separator))
	if len(parts) < 2 || parts[0] != "references" {
		return "", fmt.Errorf("reference must be inside references/")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid reference path %q", path)
		}
	}

	current := filepath.Join(c.root, name)
	skillInfo, err := os.Lstat(current)
	if err != nil {
		return "", err
	}
	if !skillInfo.IsDir() || skillInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("skill directory must not be a symlink: %q", current)
	}
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if i < len(parts)-1 {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("reference directory must not be a symlink: %q", current)
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Size() > maxReferenceFileBytes {
			return "", fmt.Errorf("reference must be a regular file of at most %d bytes", maxReferenceFileBytes)
		}
	}
	data, err := os.ReadFile(current)
	if err != nil {
		return "", err
	}
	if len(data) > maxReferenceFileBytes || !utf8.Valid(data) {
		return "", fmt.Errorf("reference must be UTF-8 text of at most %d bytes", maxReferenceFileBytes)
	}
	return string(data), nil
}

func readSkill(path string) (Metadata, string, error) {
	// 获取文件信息
	info, err := os.Lstat(path)
	if err != nil {
		return Metadata{}, "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSkillFileBytes {
		return Metadata{}, "", fmt.Errorf("SKILL.md must be a regular file of at most %d bytes", maxSkillFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Metadata{}, "", err
	}
	if !utf8.Valid(data) {
		return Metadata{}, "", fmt.Errorf("SKILL.md must be UTF-8")
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Metadata{}, "", fmt.Errorf("SKILL.md must start with YAML frontmatter")
	}
	parts := strings.SplitN(text[4:], "\n---\n", 2)
	if len(parts) != 2 {
		return Metadata{}, "", fmt.Errorf("SKILL.md frontmatter is not closed")
	}
	var metadata Metadata
	if err := yaml.Unmarshal([]byte(parts[0]), &metadata); err != nil {
		return Metadata{}, "", fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	if len(metadata.Name) > 64 || !validName.MatchString(metadata.Name) {
		return Metadata{}, "", fmt.Errorf("invalid skill name %q", metadata.Name)
	}
	metadata.Description = strings.TrimSpace(metadata.Description)
	if metadata.Description == "" || utf8.RuneCountInString(metadata.Description) > 1024 {
		return Metadata{}, "", fmt.Errorf("skill description must be 1-1024 characters")
	}
	return metadata, strings.TrimSpace(parts[1]), nil
}
