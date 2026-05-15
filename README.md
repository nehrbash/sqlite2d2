# sqlite2d2

A small CLI that converts a SQLite database schema into a [D2](https://d2lang.com) diagram.

Includes column types, `NOT NULL`, defaults, PK / FK / UNIQUE constraints, and non-trivial indexes (rendered as a markdown note pinned to each table).

## Build

```sh
cd sqlite2d2
go mod tidy
go build -o sqlite2d2 .
```

Pure Go — no CGo, so `GOOS=linux GOARCH=arm64 go build` etc. just works.

## Use

```sh
./sqlite2d2 mydb.sqlite > schema.d2
d2 --layout=elk schema.d2 schema.svg
```

Flags:

- `-o FILE` write to file instead of stdout
- `-direction up|down|left|right` D2 layout direction (default `right`)

## Notes

- Views, triggers, and virtual tables are skipped — only `CREATE TABLE` schema is rendered.
- Composite foreign keys emit one arrow per column pair (D2 has no native composite-FK concept).
- The implicit PK index and single-column UNIQUE auto-indexes aren't repeated in the notes block since they're already shown as column constraints.
- For best layout on schemas larger than ~10 tables, use the ELK engine: `d2 --layout=elk`. For really busy graphs, Terrastruct's `tala` layout (paid) tends to do an even better job.
