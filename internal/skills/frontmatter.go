package skills

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter is the skill file's YAML header (pi's SkillFrontmatter).
// Unknown keys are ignored, matching pi's pass-through struct.
type Frontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

// ParseFrontmatter splits a markdown document into its YAML frontmatter
// and body (pi's parseFrontmatter): the document must start with "---"
// and carry a closing "\n---" marker. Without a header the frontmatter
// is empty and the body is the whole document.
func ParseFrontmatter(content string) (Frontmatter, string, error) {
	yamlString, body := extractFrontmatter(content)
	if yamlString == "" {
		return Frontmatter{}, body, nil
	}
	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(yamlString), &fm); err != nil {
		return Frontmatter{}, body, err
	}
	return fm, body, nil
}

// StripFrontmatter returns the body without the YAML header (pi's
// stripFrontmatter).
func StripFrontmatter(content string) string {
	_, body, _ := ParseFrontmatter(content)
	return body
}

// extractFrontmatter is pi's extractFrontmatter: BOM stripped, newlines
// normalized, header sliced between the opening "---" and the next
// "\n---". The body is trimmed.
func extractFrontmatter(content string) (yamlString string, body string) {
	normalized := strings.ReplaceAll(strings.TrimPrefix(content, "\uFEFF"), "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	if !strings.HasPrefix(normalized, "---") {
		return "", normalized
	}
	endIndex := strings.Index(normalized[3:], "\n---")
	if endIndex == -1 {
		return "", normalized
	}
	// Empty frontmatter ("---\n---") yields endIndex == 0, where the
	// raw slice would be [4:3] — clamp like pi's substring(4, end).
	end := 3 + endIndex
	if end < 4 {
		end = 4
	}
	yamlString = normalized[4:end]
	body = strings.TrimSpace(normalized[3+endIndex+4:])
	return yamlString, body
}
