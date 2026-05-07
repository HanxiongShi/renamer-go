package main

import (
	"database/sql"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"
)

var tokenRE = regexp.MustCompile(`\{([a-zA-Z0-9_:-]+)\}`)

type schemaConfig struct {
	filesTable       string
	metadataTable    string
	idColumn         string
	pathColumn       string
	nameColumn       string
	metaFileIDColumn string
	metaKeyColumn    string
	metaValueColumn  string
}

type fileRecord struct {
	ID           int64
	DirPath      string
	OriginalName string
	Metadata     map[string]string
}

type renamePlan struct {
	From string
	To   string
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "preview":
		if err := runPreview(os.Args[2:]); err != nil {
			exitWithError(err)
		}
	case "apply":
		if err := runApply(os.Args[2:]); err != nil {
			exitWithError(err)
		}
	default:
		printUsage()
		os.Exit(2)
	}
}

func runPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	dbPath, pattern, csvPath, cfg, err := parseSharedFlags(fs, args)
	if err != nil {
		return err
	}
	if csvPath == "" {
		return errors.New("missing required --csv output path")
	}

	plans, err := buildPlans(dbPath, pattern, cfg)
	if err != nil {
		return err
	}

	if err := writeCSV(csvPath, plans); err != nil {
		return err
	}

	fmt.Printf("Wrote %d rename rows to %s\n", len(plans), csvPath)
	return nil
}

func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	dbPath, pattern, _, cfg, err := parseSharedFlags(fs, args)
	if err != nil {
		return err
	}
	dryRun := fs.Bool("dry-run", false, "Only print rename operations, do not rename files.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	plans, err := buildPlans(dbPath, pattern, cfg)
	if err != nil {
		return err
	}

	if *dryRun {
		for _, p := range plans {
			fmt.Printf("[DRY RUN] %s -> %s\n", p.From, p.To)
		}
		fmt.Printf("Dry run complete. %d files would be renamed.\n", len(plans))
		return nil
	}

	for _, p := range plans {
		if p.From == p.To {
			continue
		}
		if err := os.Rename(p.From, p.To); err != nil {
			return fmt.Errorf("rename %q -> %q: %w", p.From, p.To, err)
		}
		fmt.Printf("Renamed: %s -> %s\n", p.From, p.To)
	}

	fmt.Printf("Rename complete. %d file(s) processed.\n", len(plans))
	return nil
}

func parseSharedFlags(fs *flag.FlagSet, args []string) (dbPath, pattern, csvPath string, cfg schemaConfig, err error) {
	db := fs.String("db", "", "Path to sqlite database file.")
	pat := fs.String("pattern", "{basename}{ext}", "Rename pattern. Tokens: {original}, {basename}, {ext}, {meta:<key>}.")
	csv := fs.String("csv", "", "Path to CSV output file (used by preview).")

	cfg.filesTable = "files"
	cfg.metadataTable = "file_metadata"
	cfg.idColumn = "id"
	cfg.pathColumn = "path"
	cfg.nameColumn = "original_filename"
	cfg.metaFileIDColumn = "file_id"
	cfg.metaKeyColumn = "key"
	cfg.metaValueColumn = "value"

	fs.StringVar(&cfg.filesTable, "files-table", cfg.filesTable, "Files table name.")
	fs.StringVar(&cfg.metadataTable, "metadata-table", cfg.metadataTable, "Metadata key/value table name.")
	fs.StringVar(&cfg.idColumn, "id-column", cfg.idColumn, "Files table id column name.")
	fs.StringVar(&cfg.pathColumn, "path-column", cfg.pathColumn, "Files table directory path column name.")
	fs.StringVar(&cfg.nameColumn, "name-column", cfg.nameColumn, "Files table original filename column name.")
	fs.StringVar(&cfg.metaFileIDColumn, "meta-file-id-column", cfg.metaFileIDColumn, "Metadata table file-id column name.")
	fs.StringVar(&cfg.metaKeyColumn, "meta-key-column", cfg.metaKeyColumn, "Metadata table key column name.")
	fs.StringVar(&cfg.metaValueColumn, "meta-value-column", cfg.metaValueColumn, "Metadata table value column name.")

	if err := fs.Parse(args); err != nil {
		return "", "", "", cfg, err
	}

	if *db == "" {
		return "", "", "", cfg, errors.New("missing required --db path")
	}

	return *db, *pat, *csv, cfg, nil
}

func buildPlans(dbPath, pattern string, cfg schemaConfig) ([]renamePlan, error) {
	records, err := loadRecords(dbPath, cfg)
	if err != nil {
		return nil, err
	}

	plans := make([]renamePlan, 0, len(records))
	targets := make(map[string]string, len(records))

	for _, r := range records {
		newName := buildNewFilename(r, pattern)
		if newName == "" {
			return nil, fmt.Errorf("file id %d produced empty filename", r.ID)
		}

		from := filepath.Join(r.DirPath, r.OriginalName)
		to := filepath.Join(r.DirPath, newName)
		if current, exists := targets[to]; exists && current != from {
			return nil, fmt.Errorf("target collision: %q and %q both map to %q", current, from, to)
		}
		targets[to] = from
		plans = append(plans, renamePlan{From: from, To: to})
	}

	return plans, nil
}

func loadRecords(dbPath string, cfg schemaConfig) ([]fileRecord, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	fileSQL := fmt.Sprintf(
		`SELECT %s, %s, %s FROM %s`,
		quoteIdent(cfg.idColumn),
		quoteIdent(cfg.pathColumn),
		quoteIdent(cfg.nameColumn),
		quoteIdent(cfg.filesTable),
	)

	rows, err := db.Query(fileSQL)
	if err != nil {
		return nil, fmt.Errorf("query files: %w", err)
	}
	defer rows.Close()

	var records []fileRecord

	for rows.Next() {
		var r fileRecord
		r.Metadata = map[string]string{}
		if err := rows.Scan(&r.ID, &r.DirPath, &r.OriginalName); err != nil {
			return nil, fmt.Errorf("scan files row: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files rows error: %w", err)
	}

	indexByID := make(map[int64]int, len(records))
	for i, r := range records {
		indexByID[r.ID] = i
	}

	metaSQL := fmt.Sprintf(
		`SELECT %s, %s, %s FROM %s`,
		quoteIdent(cfg.metaFileIDColumn),
		quoteIdent(cfg.metaKeyColumn),
		quoteIdent(cfg.metaValueColumn),
		quoteIdent(cfg.metadataTable),
	)
	metaRows, err := db.Query(metaSQL)
	if err != nil {
		return nil, fmt.Errorf("query metadata: %w", err)
	}
	defer metaRows.Close()

	for metaRows.Next() {
		var fileID int64
		var key, value string
		if err := metaRows.Scan(&fileID, &key, &value); err != nil {
			return nil, fmt.Errorf("scan metadata row: %w", err)
		}
		i, ok := indexByID[fileID]
		if !ok {
			continue
		}
		records[i].Metadata[key] = value
	}
	if err := metaRows.Err(); err != nil {
		return nil, fmt.Errorf("metadata rows error: %w", err)
	}

	return records, nil
}

func buildNewFilename(r fileRecord, pattern string) string {
	ext := filepath.Ext(r.OriginalName)
	basename := strings.TrimSuffix(r.OriginalName, ext)

	repl := tokenRE.ReplaceAllStringFunc(pattern, func(match string) string {
		key := strings.TrimSuffix(strings.TrimPrefix(match, "{"), "}")
		switch {
		case key == "original":
			return r.OriginalName
		case key == "basename":
			return basename
		case key == "ext":
			return ext
		case strings.HasPrefix(key, "meta:"):
			metaKey := strings.TrimPrefix(key, "meta:")
			return r.Metadata[metaKey]
		default:
			return ""
		}
	})

	return sanitizeFilename(repl)
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, string(os.PathSeparator), "_")
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.Join(strings.Fields(name), " ")
	return name
}

func writeCSV(path string, plans []renamePlan) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"current_file", "renamed_file"}); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, p := range plans {
		if err := w.Write([]string{p.From, p.To}); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	if err := w.Error(); err != nil {
		return fmt.Errorf("flush csv: %w", err)
	}
	return nil
}

func quoteIdent(s string) string {
	escaped := strings.ReplaceAll(s, `"`, `""`)
	return `"` + escaped + `"`
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage:
  renamer-go preview --db <db.sqlite> --csv <output.csv> [options]
  renamer-go apply   --db <db.sqlite> [--dry-run] [options]

Options:
  --pattern "{meta:artist} - {meta:title}{ext}"
  --files-table files
  --metadata-table file_metadata
  --id-column id
  --path-column path
  --name-column original_filename
  --meta-file-id-column file_id
  --meta-key-column key
  --meta-value-column value
`)
}

func exitWithError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
