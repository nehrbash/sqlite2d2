// sqlite2d2 converts a SQLite database schema into a D2 diagram file.
//
// Usage:
//
//	sqlite2d2 path/to/db.sqlite              # writes D2 to stdout
//	sqlite2d2 path/to/db.sqlite -o schema.d2 # writes to file
//	sqlite2d2 -direction down db.sqlite      # change layout direction
//
// Render with:
//
//	d2 --layout=elk schema.d2 schema.svg
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

type Column struct {
	Name    string
	Type    string
	NotNull bool
	Default sql.NullString
	PK      int // 0 if not PK, else 1-based position within the PK
}

type ForeignKey struct {
	ID       int
	Seq      int
	Table    string
	From     string
	To       string
	OnUpdate string
	OnDelete string
}

type Index struct {
	Name    string
	Unique  bool
	Origin  string // "c" = CREATE INDEX, "u" = UNIQUE, "pk" = PRIMARY KEY
	Partial bool
	Columns []string
}

type Table struct {
	Name        string
	Columns     []Column
	ForeignKeys []ForeignKey
	Indexes     []Index
}

func main() {
	output := flag.String("o", "", "Output file (default: stdout)")
	direction := flag.String("direction", "right", "D2 layout direction: up|down|left|right")
	notes := flag.Bool("notes", false,
		"Emit per-table markdown index notes pinned with `near:`. "+
			"Only the TALA layout engine supports `near: <object>`; ELK and "+
			"dagre will reject it, so this is off by default.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr,
			"usage: sqlite2d2 [-o out.d2] [-direction right] [-notes] <database.sqlite>")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	dbPath := flag.Arg(0)
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", dbPath, err)
		os.Exit(1)
	}

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	tables, err := loadSchema(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load schema: %v\n", err)
		os.Exit(1)
	}

	out := renderD2(tables, *direction, *notes)

	if *output == "" {
		fmt.Print(out)
	} else if err := os.WriteFile(*output, []byte(out), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
}

func loadSchema(db *sql.DB) ([]Table, error) {
	rows, err := db.Query(`
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()

	tables := make([]Table, 0, len(names))
	for _, name := range names {
		t := Table{Name: name}

		// Columns
		cr, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, quoteSQL(name)))
		if err != nil {
			return nil, fmt.Errorf("table_info(%s): %w", name, err)
		}
		for cr.Next() {
			var cid int
			var c Column
			if err := cr.Scan(&cid, &c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK); err != nil {
				cr.Close()
				return nil, err
			}
			t.Columns = append(t.Columns, c)
		}
		cr.Close()

		// Foreign keys
		fr, err := db.Query(fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, quoteSQL(name)))
		if err != nil {
			return nil, fmt.Errorf("foreign_key_list(%s): %w", name, err)
		}
		for fr.Next() {
			var fk ForeignKey
			var match string
			if err := fr.Scan(&fk.ID, &fk.Seq, &fk.Table, &fk.From, &fk.To,
				&fk.OnUpdate, &fk.OnDelete, &match); err != nil {
				fr.Close()
				return nil, err
			}
			t.ForeignKeys = append(t.ForeignKeys, fk)
		}
		fr.Close()

		// Indexes
		ir, err := db.Query(fmt.Sprintf(`PRAGMA index_list(%s)`, quoteSQL(name)))
		if err != nil {
			return nil, fmt.Errorf("index_list(%s): %w", name, err)
		}
		var idxs []Index
		for ir.Next() {
			var seq, unique, partial int
			var idx Index
			if err := ir.Scan(&seq, &idx.Name, &unique, &idx.Origin, &partial); err != nil {
				ir.Close()
				return nil, err
			}
			idx.Unique = unique != 0
			idx.Partial = partial != 0
			idxs = append(idxs, idx)
		}
		ir.Close()

		for i := range idxs {
			iir, err := db.Query(fmt.Sprintf(`PRAGMA index_info(%s)`, quoteSQL(idxs[i].Name)))
			if err != nil {
				return nil, fmt.Errorf("index_info(%s): %w", idxs[i].Name, err)
			}
			for iir.Next() {
				var seqno, cid int
				var cname sql.NullString
				if err := iir.Scan(&seqno, &cid, &cname); err != nil {
					iir.Close()
					return nil, err
				}
				if cname.Valid {
					idxs[i].Columns = append(idxs[i].Columns, cname.String)
				}
			}
			iir.Close()
		}
		// Stable order: PK index first, then unique, then explicit
		sort.SliceStable(idxs, func(i, j int) bool {
			rank := func(o string) int {
				switch o {
				case "pk":
					return 0
				case "u":
					return 1
				default:
					return 2
				}
			}
			return rank(idxs[i].Origin) < rank(idxs[j].Origin)
		})
		t.Indexes = idxs

		tables = append(tables, t)
	}
	return tables, nil
}

func renderD2(tables []Table, direction string, notes bool) string {
	var sb strings.Builder
	sb.WriteString("# Auto-generated from SQLite schema by sqlite2d2\n")
	sb.WriteString("# Render: d2 --layout=elk schema.d2 schema.svg\n\n")
	fmt.Fprintf(&sb, "direction: %s\n\n", direction)

	for _, t := range tables {
		fkCols := map[string]bool{}
		for _, fk := range t.ForeignKeys {
			fkCols[fk.From] = true
		}
		uniqueCols := map[string]bool{}
		for _, idx := range t.Indexes {
			// Single-column UNIQUE constraint from table definition
			if idx.Unique && idx.Origin == "u" && len(idx.Columns) == 1 {
				uniqueCols[idx.Columns[0]] = true
			}
		}

		fmt.Fprintf(&sb, "%s: {\n", d2Ident(t.Name))
		sb.WriteString("  shape: sql_table\n")

		for _, c := range t.Columns {
			typeStr := strings.TrimSpace(c.Type)
			if typeStr == "" {
				typeStr = "ANY"
			}
			if c.NotNull {
				typeStr += " NOT NULL"
			}
			if c.Default.Valid {
				typeStr += " DEFAULT " + condense(c.Default.String)
			}

			var cons []string
			if c.PK > 0 {
				cons = append(cons, "primary_key")
			}
			if fkCols[c.Name] {
				cons = append(cons, "foreign_key")
			}
			if uniqueCols[c.Name] && c.PK == 0 {
				cons = append(cons, "unique")
			}

			line := fmt.Sprintf("  %s: %s", d2Ident(c.Name), typeStr)
			switch len(cons) {
			case 0:
				// nothing
			case 1:
				line += fmt.Sprintf(" {constraint: %s}", cons[0])
			default:
				line += fmt.Sprintf(" {constraint: [%s]}", strings.Join(cons, "; "))
			}
			sb.WriteString(line + "\n")
		}
		sb.WriteString("}\n\n")

		// Indexes as a markdown note pinned near the table.
		// Skip the implicit PK index ("pk") since it duplicates the PK constraint.
		// Also skip single-column unique-origin indexes already shown as {constraint: unique}.
		if !notes {
			continue
		}
		var notable []Index
		for _, idx := range t.Indexes {
			if idx.Origin == "pk" {
				continue
			}
			if idx.Origin == "u" && len(idx.Columns) == 1 {
				continue
			}
			notable = append(notable, idx)
		}
		if len(notable) > 0 {
			noteName := d2Ident(t.Name + "_indexes")
			fmt.Fprintf(&sb, "%s: |md\n", noteName)
			fmt.Fprintf(&sb, "  **Indexes on `%s`**\n\n", t.Name)
			for _, idx := range notable {
				tags := []string{}
				if idx.Unique {
					tags = append(tags, "unique")
				}
				if idx.Partial {
					tags = append(tags, "partial")
				}
				suffix := ""
				if len(tags) > 0 {
					suffix = " *(" + strings.Join(tags, ", ") + ")*"
				}
				fmt.Fprintf(&sb, "  - `%s`%s on (%s)\n",
					idx.Name, suffix, strings.Join(idx.Columns, ", "))
			}
			fmt.Fprintf(&sb, "| {\n  near: %s\n  style.fill: \"#f7f7f5\"\n}\n\n", d2Ident(t.Name))
		}
	}

	// Foreign-key arrows
	hasFK := false
	for _, t := range tables {
		if len(t.ForeignKeys) > 0 {
			hasFK = true
			break
		}
	}
	if hasFK {
		sb.WriteString("# Foreign-key relationships\n")
		for _, t := range tables {
			for _, fk := range t.ForeignKeys {
				label := ""
				if fk.OnDelete != "" && fk.OnDelete != "NO ACTION" {
					label = " : on delete " + strings.ToLower(fk.OnDelete)
				}
				fmt.Fprintf(&sb, "%s.%s -> %s.%s%s\n",
					d2Ident(t.Name), d2Ident(fk.From),
					d2Ident(fk.Table), d2Ident(fk.To), label)
			}
		}
	}

	return sb.String()
}

// d2ReservedKeys are D2 keywords that, when used as a bare identifier in
// a shape body, are interpreted as shape properties rather than as a
// column/child name. The one that bites in practice is `icon` (D2 reads
// the value as an image path and tries to bundle it). Quoting forces D2
// to treat the name as a child object.
var d2ReservedKeys = map[string]bool{
	"icon":      true,
	"shape":     true,
	"style":     true,
	"near":      true,
	"label":     true,
	"tooltip":   true,
	"link":      true,
	"classes":   true,
	"direction": true,
	"width":     true,
	"height":    true,
	"source":    true,
	"target":    true,
	"top":       true,
	"left":      true,
	"grid-rows": true,
	"grid-cols": true,
}

// d2Ident quotes an identifier if it contains characters D2 treats
// specially or collides with a D2 reserved key.
func d2Ident(name string) string {
	if name == "" {
		return `""`
	}
	if d2ReservedKeys[name] ||
		strings.ContainsAny(name, " .-/\\\t\n\"'(){}[]:;,&|<>") {
		return `"` + strings.ReplaceAll(name, `"`, `\"`) + `"`
	}
	return name
}

// quoteSQL wraps an identifier in double quotes for use inside a PRAGMA call.
func quoteSQL(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// condense collapses internal whitespace so a multi-line default doesn't break
// the D2 column line.
func condense(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
