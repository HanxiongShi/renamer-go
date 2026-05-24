# renamer-go

CLI tool for previewing and applying metadata-based file renames from a SQLite database.

## Build

```bash
go build -o renamer-go .
```

## Default schema

- `files(id, path, original_filename)`
- `file_metadata(file_id, key, value)`

Override table/column names via flags if needed.

## Preview mode (CSV)

```bash
./renamer-go preview \
  --db ./files.sqlite \
  --csv ./rename-preview.csv \
  --pattern "{meta:artist} - {meta:title}{ext}"
```

Limit to one folder (paths are relative to `/Merge` on the library volume):

```bash
./renamer-go preview \
  --db ./files.sqlite \
  --csv ./rename-preview.csv \
  --folder "/Stars/Abigaile Johnson - 2009"
```

`/Stars/...` resolves to `/Volumes/hanxiongshi/Merge/Stars/...`.

## Apply mode

Dry run:

```bash
./renamer-go apply \
  --db ./files.sqlite \
  --pattern "{meta:artist} - {meta:title}{ext}" \
  --dry-run
```

Dry run for a single folder:

```bash
./renamer-go apply \
  --db ./files.sqlite \
  --folder "/Stars/Abigaile Johnson - 2009" \
  --dry-run
```

Real rename:

```bash
./renamer-go apply \
  --db ./files.sqlite \
  --pattern "{meta:artist} - {meta:title}{ext}"
```

## Pattern tokens

- `{original}`: original filename
- `{basename}`: original filename without extension
- `{ext}`: extension with dot
- `{meta:<key>}`: metadata value by key
