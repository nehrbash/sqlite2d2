package main

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const testSchema = `
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    name TEXT,
    icon TEXT,                       -- D2-reserved key; must be quoted
    style TEXT,                      -- another D2-reserved key
    created_at TEXT DEFAULT CURRENT_TIMESTAMP,
    status TEXT NOT NULL DEFAULT 'active'
);

CREATE TABLE posts (
    id INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    body TEXT,
    published INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_posts_user ON posts(user_id);
CREATE INDEX idx_posts_published ON posts(published) WHERE published = 1;

CREATE TABLE tags (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE post_tags (
    post_id INTEGER NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (post_id, tag_id)
);

CREATE VIEW active_users AS SELECT * FROM users WHERE status = 'active';
CREATE TRIGGER bump_modified AFTER UPDATE ON posts BEGIN SELECT 1; END;
`

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatalf("exec schema: %v", err)
	}
	return db
}

func TestLoadSchema(t *testing.T) {
	db := openTestDB(t)
	tables, err := loadSchema(db)
	if err != nil {
		t.Fatalf("loadSchema: %v", err)
	}

	want := map[string]bool{"users": true, "posts": true, "tags": true, "post_tags": true}
	got := map[string]Table{}
	for _, tbl := range tables {
		got[tbl.Name] = tbl
	}
	if len(got) != len(want) {
		t.Fatalf("table count = %d, want %d (got %v)", len(got), len(want), keys(got))
	}
	for n := range want {
		if _, ok := got[n]; !ok {
			t.Errorf("missing table %q", n)
		}
	}

	// Views and triggers must be skipped.
	if _, ok := got["active_users"]; ok {
		t.Errorf("view active_users should be skipped")
	}

	users := got["users"]
	if len(users.Columns) != 7 {
		t.Errorf("users columns = %d, want 7", len(users.Columns))
	}
	if users.Columns[0].Name != "id" || users.Columns[0].PK != 1 {
		t.Errorf("users.id should be PK, got %+v", users.Columns[0])
	}
	if !users.Columns[1].NotNull || users.Columns[1].Name != "email" {
		t.Errorf("users.email should be NOT NULL, got %+v", users.Columns[1])
	}
	// `created_at` shifted from index 3 to index 5 after adding icon/style.
	if !users.Columns[5].Default.Valid || users.Columns[5].Default.String == "" {
		t.Errorf("users.created_at should have default, got %+v", users.Columns[5])
	}

	posts := got["posts"]
	if len(posts.ForeignKeys) != 1 || posts.ForeignKeys[0].Table != "users" ||
		posts.ForeignKeys[0].From != "user_id" || posts.ForeignKeys[0].To != "id" ||
		posts.ForeignKeys[0].OnDelete != "CASCADE" {
		t.Errorf("posts FK wrong: %+v", posts.ForeignKeys)
	}

	// Two explicit indexes plus the implicit PK index.
	var explicit, partial int
	for _, idx := range posts.Indexes {
		if idx.Origin == "c" {
			explicit++
		}
		if idx.Partial {
			partial++
		}
	}
	if explicit != 2 {
		t.Errorf("posts explicit indexes = %d, want 2", explicit)
	}
	if partial != 1 {
		t.Errorf("posts partial indexes = %d, want 1", partial)
	}

	// Composite PK on post_tags.
	pt := got["post_tags"]
	pkCount := 0
	for _, c := range pt.Columns {
		if c.PK > 0 {
			pkCount++
		}
	}
	if pkCount != 2 {
		t.Errorf("post_tags composite PK columns = %d, want 2", pkCount)
	}
	if len(pt.ForeignKeys) != 2 {
		t.Errorf("post_tags FKs = %d, want 2", len(pt.ForeignKeys))
	}
}

func TestRenderD2(t *testing.T) {
	db := openTestDB(t)
	tables, err := loadSchema(db)
	if err != nil {
		t.Fatalf("loadSchema: %v", err)
	}
	out := renderD2(tables, "right", true)

	mustContain := []string{
		"direction: right",
		"users: {",
		"shape: sql_table",
		"id: INTEGER {constraint: primary_key}",
		"email: TEXT NOT NULL {constraint: unique}",
		"status: TEXT NOT NULL DEFAULT 'active'",
		"user_id: INTEGER NOT NULL {constraint: foreign_key}",
		"{constraint: [primary_key; foreign_key]}",
		// D2-reserved column names must be quoted so D2 doesn't interpret
		// them as shape properties (`icon:` would trigger an image bundle).
		`"icon": TEXT`,
		`"style": TEXT`,
		// notes=true emits index markdown blocks.
		"posts_indexes: |md",
		"**Indexes on `posts`**",
		"`idx_posts_published` *(partial)* on (published)",
		"`idx_posts_user` on (user_id)",
		"# Foreign-key relationships",
		"posts.user_id -> users.id : on delete cascade",
		"post_tags.post_id -> posts.id : on delete cascade",
		"post_tags.tag_id -> tags.id : on delete cascade",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q\n--- output ---\n%s", s, out)
		}
	}

	// Bare `icon:` at column indent would break ELK/dagre (D2 treats it
	// as a shape icon-image path). The reserved-name quoting must catch it.
	if strings.Contains(out, "\n  icon: ") {
		t.Errorf("unquoted reserved column `icon:` leaked into output:\n%s", out)
	}

	// PK index ("pk") and single-column unique-origin indexes must not appear
	// in the markdown notes blocks.
	if strings.Contains(out, "sqlite_autoindex") {
		t.Errorf("autoindex should not appear in output")
	}
	// Implicit PK index is named like sqlite_autoindex_*; covered above.
	// Notes block for users should not exist (only unique-origin single-col index).
	if strings.Contains(out, "users_indexes: |md") {
		t.Errorf("users should have no notable indexes")
	}
}

// notes=false (the default) must omit every `*_indexes: |md` block, since
// they use `near: <table>` which ELK and dagre reject.
func TestRenderD2_NotesDisabled(t *testing.T) {
	db := openTestDB(t)
	tables, err := loadSchema(db)
	if err != nil {
		t.Fatalf("loadSchema: %v", err)
	}
	out := renderD2(tables, "right", false)

	if strings.Contains(out, "_indexes: |md") {
		t.Errorf("notes=false should suppress _indexes blocks; got:\n%s", out)
	}
	if strings.Contains(out, "near:") {
		t.Errorf("notes=false should not emit any `near:` directives; got:\n%s", out)
	}
	// Sanity: the rest of the schema still renders.
	if !strings.Contains(out, "posts: {") {
		t.Errorf("posts table missing from output:\n%s", out)
	}
}

func TestD2Ident(t *testing.T) {
	cases := map[string]string{
		"simple":      "simple",
		"":            `""`,
		"with space":  `"with space"`,
		"dash-name":   `"dash-name"`,
		`has"quote`:   `"has\"quote"`,
		"snake_case2": "snake_case2",
		// D2 reserved keys must be quoted even when otherwise-valid identifiers,
		// so D2 reads them as child object names rather than shape properties.
		"icon":      `"icon"`,
		"shape":     `"shape"`,
		"style":     `"style"`,
		"label":     `"label"`,
		"near":      `"near"`,
		"direction": `"direction"`,
	}
	for in, want := range cases {
		if got := d2Ident(in); got != want {
			t.Errorf("d2Ident(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCondense(t *testing.T) {
	if got := condense("  a\n\tb   c "); got != "a b c" {
		t.Errorf("condense = %q", got)
	}
}

func keys(m map[string]Table) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
