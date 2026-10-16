package arch

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The team's PostgreSQL conventions, checked on every db/*.sql. An intentional deviation is
// declared on the offending line (or the line above it): `-- ddl-allow: <rule>  <reason>`.
// A vendored file nobody may rewrite is exempted whole in its first lines: `-- ddl-allow-file: <reason>`.
// A rule fires when pattern matches and unless (optional) does not: Go's RE2 has no lookahead.
var ddlRules = []struct {
	id      string
	pattern *regexp.Regexp
	unless  *regexp.Regexp
	message string
}{
	{"varchar", regexp.MustCompile(`(?i)\bvarchar\b|\bcharacter varying\b|\bchar\(`), nil,
		"strings are text; length checks live in code"},
	{"timestamp-without-tz", regexp.MustCompile(`(?i)\btimestamp\b`), regexp.MustCompile(`(?i)timestamptz|with\s+time\s+zone`),
		"use TIMESTAMPTZ (UTC)"},
	{"serial-primary-key", regexp.MustCompile(`(?i)\bbigserial\b`), nil,
		"primary keys are SERIAL (int), BIGSERIAL only for flow tables"},
	{"pg-enum", regexp.MustCompile(`(?i)\bcreate\s+type\b.*\bas\s+enum\b`), nil,
		"enumerations are text constrained by code"},
	{"physical-delete", regexp.MustCompile(`(?i)^\s*(delete\s+from|drop\s+table|truncate)\b`), nil,
		"rows are soft-deleted by status; tables are never dropped in a release script"},
	{"if-not-exists", regexp.MustCompile(`(?i)^\s*create\s+(table|index|unique\s+index)\s+`), regexp.MustCompile(`(?i)if\s+not\s+exists`),
		"DDL must be re-runnable: CREATE ... IF NOT EXISTS"},
	{"reserved-word", regexp.MustCompile(`(?i)^\s*create\s+table\s+(if\s+not\s+exists\s+)?(order|user|group|before|after|limit|offset|table|select)\s*\(`), nil,
		"the table name is a reserved word"},
	{"money-type", regexp.MustCompile(`(?i)\bmoney\b`), nil, "money is numeric(10,2)"},
}

var singularTable = regexp.MustCompile(`(?i)^\s*create\s+table\s+(?:if\s+not\s+exists\s+)?([a-z_]+)`)
var foreignKey = regexp.MustCompile(`(?i)\bforeign\s+key\s*\(([a-z_]+)\)`)
var columnLine = regexp.MustCompile(`(?i)^\s*([a-z_]+)\s+[a-z]+.*`)

func TestDDLConventions(t *testing.T) {
	files, err := filepath.Glob("../../db/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no db/*.sql found: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) { checkFile(t, file) })
	}
}

func checkFile(t *testing.T, file string) {
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	for _, l := range lines[:min(5, len(lines))] {
		if strings.Contains(l, "-- ddl-allow-file:") {
			return
		}
	}

	nullableColumns := map[string]bool{}
	for i, line := range lines {
		code := stripComment(line)
		if strings.TrimSpace(code) == "" {
			continue
		}
		where := fmt.Sprintf("%s:%d", filepath.Base(file), i+1)
		allowed := allowances(lines, i)
		checkRules(t, where, code, allowed)
		checkTables(t, where, code, allowed, nullableColumns)
	}
}

func checkRules(t *testing.T, where, code string, allowed map[string]bool) {
	t.Helper()
	for _, rule := range ddlRules {
		if rule.unless != nil && rule.unless.MatchString(code) {
			continue
		}
		if rule.pattern.MatchString(code) && !allowed[rule.id] {
			t.Errorf("%s: [%s] %s", where, rule.id, rule.message)
		}
	}
}

var (
	nullable = regexp.MustCompile(`(?i)\bnull\b`)
	notNull  = regexp.MustCompile(`(?i)\bnot\s+null\b`)
)

func checkTables(t *testing.T, where, code string, allowed, nullableColumns map[string]bool) {
	t.Helper()
	if m := singularTable.FindStringSubmatch(code); m != nil && !strings.HasSuffix(m[1], "s") && !allowed["plural-table"] {
		t.Errorf("%s: [plural-table] table %q must be plural", where, m[1])
	}
	if m := columnLine.FindStringSubmatch(code); m != nil && nullable.MatchString(code) && !notNull.MatchString(code) {
		nullableColumns[m[1]] = true
	}
	if m := foreignKey.FindStringSubmatch(code); m != nil && nullableColumns[m[1]] && !allowed["fk-not-null"] {
		t.Errorf("%s: [fk-not-null] foreign key column %q must be NOT NULL", where, m[1])
	}
}

func stripComment(line string) string {
	if i := strings.Index(line, "--"); i >= 0 {
		return line[:i]
	}
	return line
}

func allowances(lines []string, i int) map[string]bool {
	out := map[string]bool{}
	for _, l := range []string{lines[i], lines[max(i-1, 0)]} {
		if j := strings.Index(l, "-- ddl-allow:"); j >= 0 {
			fields := strings.Fields(l[j+len("-- ddl-allow:"):])
			if len(fields) > 0 {
				out[fields[0]] = true
			}
		}
	}
	return out
}
