package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	goldenTableRE = regexp.MustCompile(`(?m)^CREATE TABLE ([a-z_]+) \(`)
	numberWords   = map[string]int{"Fourteen": 14, "Fifteen": 15, "Sixteen": 16, "Seventeen": 17, "Eighteen": 18, "Nineteen": 19, "Twenty": 20}
	tableCountRE  = regexp.MustCompile(`(?m)^([A-Z][a-z]+) tables`)
)

// TestDesignNamesEveryTable: the design's §4 data model names every table
// the golden schema has, and its count is the golden's. It said "Six tables"
// over a listing of fourteen while the schema held sixteen, and nothing
// noticed. The control is that the golden parses to tables at all, and that
// the check finds a table it is given that §4 does not name.
func TestDesignNamesEveryTable(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join(root, "internal/store/testdata/schema.golden"))
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for _, m := range goldenTableRE.FindAllStringSubmatch(string(golden), -1) {
		tables = append(tables, m[1])
	}
	if len(tables) < 10 {
		t.Fatalf("control: the golden schema parsed to %d tables: %v", len(tables), tables)
	}

	doc, err := os.ReadFile(filepath.Join(root, "docs/design/overall/drydock-design.md"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(doc), "\n## 4. Data model\n")
	end := strings.Index(string(doc), "\n## 5.")
	if start < 0 || end < start {
		t.Fatal("cannot find §4 in the design document")
	}
	section := string(doc[start:end])

	names := func(table string) bool {
		return regexp.MustCompile(`(?m)(^` + table + `\(|` + "`" + table + "[ `(])").MatchString(section)
	}
	if names("no_such_table") {
		t.Fatal("control: the check finds a table §4 does not name")
	}
	for _, table := range tables {
		if !names(table) {
			t.Errorf("§4 does not name the table %s, which the golden schema has", table)
		}
	}

	m := tableCountRE.FindStringSubmatch(section)
	if m == nil {
		t.Fatal(`§4 no longer opens with "<Number> tables"`)
	}
	if n, ok := numberWords[m[1]]; !ok || n != len(tables) {
		t.Errorf("§4 says %q tables; the golden schema has %d", m[1], len(tables))
	}
}
