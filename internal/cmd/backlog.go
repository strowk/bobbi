package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"bobbi/internal/queue"

	"gopkg.in/yaml.v3"
)

// backlogFrontmatter represents the YAML frontmatter of a backlog item.
type backlogFrontmatter struct {
	Created  time.Time `yaml:"created"`
	Type     string    `yaml:"type,omitempty"`
	Priority string    `yaml:"priority,omitempty"`
}

// backlogItem represents a parsed backlog file.
type backlogItem struct {
	Filename    string
	Frontmatter backlogFrontmatter
	Body        string
	Title       string // extracted from first # heading
}

func Backlog(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".bobbi")); os.IsNotExist(err) {
		return fmt.Errorf("not a bobbi project directory (run 'bobbi init' first)")
	}

	backlogDir := filepath.Join(cwd, ".bobbi", "backlog")

	if len(args) == 0 {
		return backlogList(backlogDir)
	}

	switch args[0] {
	case "add":
		return backlogAdd(backlogDir, args[1:])
	case "promote":
		if len(args) < 2 {
			return fmt.Errorf("usage: bobbi backlog promote <filename>")
		}
		queuesDir := filepath.Join(cwd, ".bobbi", "queues")
		return backlogPromote(backlogDir, queuesDir, args[1])
	case "drop":
		if len(args) < 2 {
			return fmt.Errorf("usage: bobbi backlog drop <filename>")
		}
		return backlogDrop(backlogDir, args[1])
	default:
		return fmt.Errorf("unknown backlog subcommand %q\nUsage: bobbi backlog [add|promote|drop]", args[0])
	}
}

func backlogList(backlogDir string) error {
	items, err := readBacklogItems(backlogDir)
	if err != nil {
		return err
	}

	if len(items) == 0 {
		fmt.Println("No backlog items.")
		return nil
	}

	for _, item := range items {
		title := item.Title
		if title == "" {
			title = item.Filename
		}
		typeStr := item.Frontmatter.Type
		if typeStr == "" {
			typeStr = "-"
		}
		priorityStr := item.Frontmatter.Priority
		if priorityStr == "" {
			priorityStr = "-"
		}
		created := item.Frontmatter.Created.Format("2006-01-02")
		fmt.Printf("  %-30s  type:%-8s  priority:%-6s  created:%s\n", title, typeStr, priorityStr, created)
	}

	return nil
}

func backlogAdd(backlogDir string, args []string) error {
	if err := os.MkdirAll(backlogDir, 0755); err != nil {
		return fmt.Errorf("create backlog dir: %w", err)
	}

	var body string
	if len(args) > 0 {
		body = strings.Join(args, " ")
	} else {
		var err error
		body, err = ReadInteractiveText("Describe the backlog item...")
		if err != nil {
			return err
		}
	}

	if body == "" {
		return fmt.Errorf("backlog item cannot be empty")
	}

	// Extract the first line as the title and ensure it becomes a # heading
	// so parseBacklogFile can extract the display title (CONTRACT: INTERFACES.md "Backlog").
	firstLine := strings.SplitN(body, "\n", 2)[0]
	if !strings.HasPrefix(strings.TrimSpace(firstLine), "# ") {
		// Prepend the first line as a heading; keep the rest of the body after it
		parts := strings.SplitN(body, "\n", 2)
		if len(parts) > 1 {
			body = "# " + strings.TrimSpace(parts[0]) + "\n" + parts[1]
		} else {
			body = "# " + strings.TrimSpace(parts[0])
		}
	}

	fm := backlogFrontmatter{
		Created: time.Now().UTC(),
	}
	fmData, err := yaml.Marshal(&fm)
	if err != nil {
		return fmt.Errorf("marshal frontmatter: %w", err)
	}

	content := fmt.Sprintf("---\n%s---\n%s\n", string(fmData), body)

	// Derive filename slug from the first line (without the # prefix)
	slug := slugify(strings.TrimPrefix(strings.TrimSpace(firstLine), "# "))
	if slug == "" {
		slug = "backlog-item"
	}

	filename := slug + ".md"
	path := filepath.Join(backlogDir, filename)

	// Handle collisions
	if _, err := os.Stat(path); err == nil {
		found := false
		for i := 2; i < 1000; i++ {
			filename = fmt.Sprintf("%s-%d.md", slug, i)
			path = filepath.Join(backlogDir, filename)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("could not find a free filename for slug %q after 998 attempts", slug)
		}
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write backlog item: %w", err)
	}

	fmt.Printf("[bobbi] Backlog item added: %s\n", filename)
	return nil
}

func backlogPromote(backlogDir, queuesDir string, filename string) error {
	path := filepath.Join(backlogDir, filename)

	item, err := parseBacklogFile(path)
	if err != nil {
		return fmt.Errorf("read backlog item: %w", err)
	}

	itemType := item.Frontmatter.Type

	// If type not set, prompt interactively
	if itemType == "" {
		itemType, err = promptBacklogType()
		if err != nil {
			return err
		}
	}

	// Map type to queue request type
	var reqType string
	switch itemType {
	case "bug":
		reqType = "request_solution_change"
	case "spec":
		reqType = "request_architecture_change"
	case "feature":
		// Feature: ask user to choose
		reqType, err = promptFeatureTarget()
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown backlog item type: %s", itemType)
	}

	// Strip the leading # heading from the body before sending as additional_context,
	// since the heading is display metadata, not actionable content.
	context := stripLeadingHeading(item.Body)
	_, err = queue.WriteRequest(queuesDir, reqType, "user", context)
	if err != nil {
		return fmt.Errorf("write queue request: %w", err)
	}

	// Delete the backlog file
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove backlog item: %w", err)
	}

	fmt.Printf("[bobbi] Backlog item promoted to queue: %s -> %s\n", filename, reqType)
	return nil
}

func backlogDrop(backlogDir string, filename string) error {
	path := filepath.Join(backlogDir, filename)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("backlog item not found: %s", filename)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove backlog item: %w", err)
	}
	fmt.Printf("[bobbi] Backlog item dropped: %s\n", filename)
	return nil
}

// readBacklogItems reads all .md files in the backlog directory.
func readBacklogItems(backlogDir string) ([]backlogItem, error) {
	entries, err := os.ReadDir(backlogDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read backlog dir: %w", err)
	}

	var items []backlogItem
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(backlogDir, entry.Name())
		item, err := parseBacklogFile(path)
		if err != nil {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

// parseBacklogFile parses a backlog markdown file with YAML frontmatter.
func parseBacklogFile(path string) (backlogItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return backlogItem{}, err
	}

	content := string(data)
	var fm backlogFrontmatter
	var body string

	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---\n")
		if end >= 0 {
			fmStr := content[4 : 4+end]
			body = strings.TrimSpace(content[4+end+5:])
			if err := yaml.Unmarshal([]byte(fmStr), &fm); err != nil {
				return backlogItem{}, fmt.Errorf("parse frontmatter: %w", err)
			}
		} else {
			body = content
		}
	} else {
		body = content
	}

	// Extract title from first # heading
	title := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			title = strings.TrimPrefix(trimmed, "# ")
			break
		}
	}
	if title == "" {
		// Fall back to filename
		title = filepath.Base(path)
	}

	return backlogItem{
		Filename:    filepath.Base(path),
		Frontmatter: fm,
		Body:        body,
		Title:       title,
	}, nil
}

// stripLeadingHeading removes the first # heading line from a markdown body.
func stripLeadingHeading(body string) string {
	lines := strings.SplitN(body, "\n", 2)
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "# ") {
		if len(lines) > 1 {
			return strings.TrimSpace(lines[1])
		}
		return ""
	}
	return body
}

// slugify converts a string into a URL-friendly slug.
func slugify(s string) string {
	s = strings.ToLower(s)
	// Replace spaces and punctuation with hyphens
	reg := regexp.MustCompile(`[^a-z0-9]+`)
	s = reg.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	// Limit length
	if len(s) > 60 {
		s = s[:60]
		s = strings.TrimRight(s, "-")
	}
	return s
}

// promptBacklogType asks the user to choose a type for the backlog item.
func promptBacklogType() (string, error) {
	fmt.Println("Choose item type:")
	fmt.Println("  1) bug")
	fmt.Println("  2) spec")
	fmt.Println("  3) feature")
	fmt.Print("Choice [1/2/3]: ")

	reader := bufio.NewReader(os.Stdin)
	choice, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read choice: %w", err)
	}
	choice = strings.TrimSpace(choice)

	switch choice {
	case "1", "bug":
		return "bug", nil
	case "2", "spec":
		return "spec", nil
	case "3", "feature":
		return "feature", nil
	default:
		return "", fmt.Errorf("invalid choice: %s", choice)
	}
}

// promptFeatureTarget asks the user to choose a target for a feature item.
func promptFeatureTarget() (string, error) {
	fmt.Println("Feature target:")
	fmt.Println("  1) request_solution_change")
	fmt.Println("  2) request_architecture_change")
	fmt.Print("Choice [1/2]: ")

	reader := bufio.NewReader(os.Stdin)
	choice, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read choice: %w", err)
	}
	choice = strings.TrimSpace(choice)

	switch choice {
	case "1":
		return "request_solution_change", nil
	case "2":
		return "request_architecture_change", nil
	default:
		return "", fmt.Errorf("invalid choice: %s", choice)
	}
}
