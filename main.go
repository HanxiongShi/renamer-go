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

// mergeRoot is the absolute library root; --folder paths are resolved under Merge/.
const mergeRoot = "/Volumes/hanxiongshi/Merge"

type schemaConfig struct {
	filesTable       string
	foldersTable     string
	metadataTable    string
	idColumn         string
	fileNameColumn   string
	fileFolderIDCol  string
	folderIDColumn   string
	folderPathColumn string
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
	case "list":
		if err := runList(os.Args[2:]); err != nil {
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

	dbPath, _, csvPath, _, folderAbs, err := parseSharedFlags(fs, args)
	if err != nil {
		return err
	}
	if csvPath == "" {
		return errors.New("missing required --csv output path")
	}

	plans, err := buildScenePreviewPlans(dbPath)
	if err != nil {
		return err
	}
	plans = filterPlansByFolder(plans, folderAbs)

	if err := writeCSV(csvPath, plans); err != nil {
		return err
	}

	if folderAbs != "" {
		fmt.Printf("Wrote %d rename rows to %s (folder: %s)\n", len(plans), csvPath, folderAbs)
	} else {
		fmt.Printf("Wrote %d rename rows to %s\n", len(plans), csvPath)
	}
	return nil
}

func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	dryRun := fs.Bool("dry-run", false, "Only print rename operations, do not rename files.")
	dbPath, _, _, _, folderAbs, err := parseSharedFlags(fs, args)
	if err != nil {
		return err
	}

	plans, err := buildScenePreviewPlans(dbPath)
	if err != nil {
		return err
	}
	plans = filterPlansByFolder(plans, folderAbs)

	skipped := 0
	renamed := 0
	notFound := 0

	if *dryRun {
		for _, p := range plans {
			if p.To == "" {
				skipped++
				continue
			}
			if _, err := os.Stat(p.From); errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "Skipping (not found): %s\n", p.From)
				notFound++
				continue
			}
			fmt.Printf("[DRY RUN] %s -> %s\n", p.From, p.To)
			renamed++
		}
		printApplySummary(folderAbs, renamed, skipped, notFound, true)
		return nil
	}

	for _, p := range plans {
		if p.To == "" {
			skipped++
			continue
		}
		if p.From == p.To {
			continue
		}
		if _, err := os.Stat(p.From); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "Skipping (not found): %s\n", p.From)
			notFound++
			continue
		}
		if err := os.Rename(p.From, p.To); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "Skipping (not found): %s\n", p.From)
				notFound++
				continue
			}
			return fmt.Errorf("rename %q -> %q: %w", p.From, p.To, err)
		}
		renamed++
		fmt.Printf("Renamed: %s -> %s\n", p.From, p.To)
	}

	printApplySummary(folderAbs, renamed, skipped, notFound, false)
	return nil
}

func printApplySummary(folderAbs string, renamed, skipped, notFound int, dryRun bool) {
	prefix := "Rename complete"
	suffix := fmt.Sprintf("%d file(s) renamed, %d skipped (insufficient metadata)", renamed, skipped)
	if dryRun {
		prefix = "Dry run complete"
		suffix = fmt.Sprintf("%d files would be renamed, %d skipped (insufficient metadata)", renamed, skipped)
	}
	if notFound > 0 {
		suffix += fmt.Sprintf(", %d not found", notFound)
	}
	if folderAbs != "" {
		fmt.Printf("%s (folder: %s). %s.\n", prefix, folderAbs, suffix)
	} else {
		fmt.Printf("%s. %s.\n", prefix, suffix)
	}
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	dbPath, _, _, cfg, _, err := parseSharedFlags(fs, args)
	if err != nil {
		return err
	}

	records, err := loadRecords(dbPath, cfg)
	if err != nil {
		return err
	}

	for _, r := range records {
		if r.DirPath == "" {
			fmt.Println(r.OriginalName)
			continue
		}
		fmt.Println(filepath.Join(r.DirPath, r.OriginalName))
	}
	fmt.Printf("Listed %d file(s).\n", len(records))
	return nil
}

func parseSharedFlags(fs *flag.FlagSet, args []string) (dbPath, pattern, csvPath string, cfg schemaConfig, folderAbs string, err error) {
	db := fs.String("db", "", "Path to sqlite database file.")
	pat := fs.String("pattern", "{basename}{ext}", "Rename pattern. Tokens: {original}, {basename}, {ext}, {meta:<key>}.")
	csv := fs.String("csv", "", "Path to CSV output file (used by preview).")
	folder := fs.String("folder", "", "Only process files in this folder (relative to /Merge, e.g. /Stars or Stars/Album).")

	cfg.filesTable = "files"
	cfg.foldersTable = "folders"
	cfg.metadataTable = "file_metadata"
	cfg.idColumn = "id"
	cfg.fileNameColumn = "basename"
	cfg.fileFolderIDCol = "parent_folder_id"
	cfg.folderIDColumn = "id"
	cfg.folderPathColumn = "path"
	cfg.metaFileIDColumn = "file_id"
	cfg.metaKeyColumn = "key"
	cfg.metaValueColumn = "value"

	fs.StringVar(&cfg.filesTable, "files-table", cfg.filesTable, "Files table name.")
	fs.StringVar(&cfg.foldersTable, "folders-table", cfg.foldersTable, "Folders table name.")
	fs.StringVar(&cfg.metadataTable, "metadata-table", cfg.metadataTable, "Metadata key/value table name.")
	fs.StringVar(&cfg.idColumn, "id-column", cfg.idColumn, "Files table id column name.")
	fs.StringVar(&cfg.fileNameColumn, "name-column", cfg.fileNameColumn, "Files table filename column name.")
	fs.StringVar(&cfg.fileFolderIDCol, "parent-folder-id-column", cfg.fileFolderIDCol, "Files table parent folder id column name.")
	fs.StringVar(&cfg.folderIDColumn, "folder-id-column", cfg.folderIDColumn, "Folders table id column name.")
	fs.StringVar(&cfg.folderPathColumn, "folder-path-column", cfg.folderPathColumn, "Folders table path column name.")
	fs.StringVar(&cfg.metaFileIDColumn, "meta-file-id-column", cfg.metaFileIDColumn, "Metadata table file-id column name.")
	fs.StringVar(&cfg.metaKeyColumn, "meta-key-column", cfg.metaKeyColumn, "Metadata table key column name.")
	fs.StringVar(&cfg.metaValueColumn, "meta-value-column", cfg.metaValueColumn, "Metadata table value column name.")

	if err := fs.Parse(args); err != nil {
		return "", "", "", cfg, "", err
	}

	if *db == "" {
		return "", "", "", cfg, "", errors.New("missing required --db path")
	}

	folderAbs, err = resolveFolderFilter(*folder)
	if err != nil {
		return "", "", "", cfg, "", err
	}

	return *db, *pat, *csv, cfg, folderAbs, nil
}

// resolveFolderFilter maps a --folder value to an absolute directory under mergeRoot.
// Examples: "/Stars" -> "/Volumes/hanxiongshi/Merge/Stars"
func resolveFolderFilter(folder string) (string, error) {
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return "", nil
	}

	cleaned := filepath.Clean(folder)
	merge := filepath.Clean(mergeRoot)

	if cleaned == merge {
		return merge, nil
	}
	if strings.HasPrefix(cleaned, merge+string(os.PathSeparator)) {
		return cleaned, nil
	}

	rel := strings.TrimPrefix(cleaned, string(filepath.Separator))
	rel = strings.TrimPrefix(rel, "Merge"+string(os.PathSeparator))
	if rel == "Merge" {
		return merge, nil
	}
	if rel == "" {
		return "", fmt.Errorf("invalid --folder %q", folder)
	}

	return filepath.Join(merge, rel), nil
}

func filterPlansByFolder(plans []renamePlan, folderAbs string) []renamePlan {
	if folderAbs == "" {
		return plans
	}
	folderAbs = filepath.Clean(folderAbs)
	filtered := make([]renamePlan, 0, len(plans))
	for _, p := range plans {
		if filepath.Clean(filepath.Dir(p.From)) == folderAbs {
			filtered = append(filtered, p)
		}
	}
	return filtered
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

func buildScenePreviewPlans(dbPath string) ([]renamePlan, error) {
	db, err := openReadOnlyDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	const query = `
WITH performer_names AS (
  SELECT
    ps.scene_id,
    GROUP_CONCAT(p.name, ', ') AS performer_name
  FROM performers_scenes ps
  JOIN performers p ON p.id = ps.performer_id
  GROUP BY ps.scene_id
)
SELECT
  f.id AS file_id,
  fol.path AS dir_path,
  f.basename,
  pn.performer_name,
  date(s.date) AS scene_date,
  st.name AS studio_name,
  COALESCE(vf.format, '') AS file_format
FROM files f
JOIN folders fol ON fol.id = f.parent_folder_id
JOIN scenes_files sf ON sf.file_id = f.id
JOIN scenes s ON s.id = sf.scene_id
LEFT JOIN performer_names pn ON pn.scene_id = s.id
LEFT JOIN studios st ON st.id = s.studio_id
LEFT JOIN video_files vf ON vf.file_id = f.id
WHERE sf."primary" = 1
ORDER BY f.id
`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query scene preview rows: %w", err)
	}
	defer rows.Close()

	var plans []renamePlan
	targets := map[string]string{}

	for rows.Next() {
		var fileID int64
		var dirPath, basename, fileFormat string
		var performerName, sceneDate, studioName sql.NullString
		if err := rows.Scan(&fileID, &dirPath, &basename, &performerName, &sceneDate, &studioName, &fileFormat); err != nil {
			return nil, fmt.Errorf("scan scene preview row: %w", err)
		}

		dirPath = normalizeDirPath(dirPath)
		current := filepath.Join(dirPath, basename)
		name := strings.TrimSpace(performerName.String)
		date := strings.TrimSpace(sceneDate.String)
		studio := strings.TrimSpace(studioName.String)

		// Only produce a rename when performer is present and at least one of date/studio exists.
		if name == "" || (date == "" && studio == "") {
			plans = append(plans, renamePlan{From: current, To: ""})
			continue
		}

		if date == "" {
			date = "Unknown Date"
		}
		if studio == "" {
			studio = "Unknown Studio"
		}

		ext := deriveExtension(basename, fileFormat)
		newName := sanitizeFilename(fmt.Sprintf("%s - %s - %s%s", name, date, studio, ext))
		target := filepath.Join(dirPath, newName)

		if existing, ok := targets[target]; ok && existing != current {
			newName = withFileIDSuffix(newName, fileID)
			target = filepath.Join(dirPath, newName)
		}
		targets[target] = current
		plans = append(plans, renamePlan{From: current, To: target})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scene preview rows error: %w", err)
	}

	return plans, nil
}

func loadRecords(dbPath string, cfg schemaConfig) ([]fileRecord, error) {
	db, err := openReadOnlyDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	fileSQL := fmt.Sprintf(
		`SELECT f.%s, COALESCE(d.%s, ''), f.%s
		 FROM %s AS f
		 LEFT JOIN %s AS d
		   ON d.%s = f.%s`,
		quoteIdent(cfg.idColumn),
		quoteIdent(cfg.folderPathColumn),
		quoteIdent(cfg.fileNameColumn),
		quoteIdent(cfg.filesTable),
		quoteIdent(cfg.foldersTable),
		quoteIdent(cfg.folderIDColumn),
		quoteIdent(cfg.fileFolderIDCol),
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
		r.DirPath = normalizeDirPath(r.DirPath)
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
		// Some databases (including Stash defaults) may not include metadata.
		// Allow core file/path loading to proceed without metadata.
		if strings.Contains(err.Error(), "no such table") {
			return records, nil
		}
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

func normalizeDirPath(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return dir
	}
	cleaned := filepath.Clean(dir)
	if filepath.IsAbs(cleaned) {
		return cleaned
	}
	return string(os.PathSeparator) + cleaned
}

func deriveExtension(basename, format string) string {
	if strings.TrimSpace(format) != "" {
		return "." + strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), "."))
	}
	if ext := filepath.Ext(basename); ext != "" {
		return strings.ToLower(ext)
	}
	return ""
}

func withFileIDSuffix(name string, fileID int64) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s (%d)%s", stem, fileID, ext)
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

func openReadOnlyDB(dbPath string) (*sql.DB, error) {
	abs, err := resolveDBPath(dbPath)
	if err != nil {
		return nil, err
	}

	dsn := "file:" + filepath.ToSlash(abs) + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", abs, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", abs, err)
	}
	return db, nil
}

func resolveDBPath(dbPath string) (string, error) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return "", errors.New("empty database path")
	}

	if strings.HasPrefix(dbPath, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		switch dbPath {
		case "~":
			return "", errors.New("database path must be a file, not ~")
		default:
			if !strings.HasPrefix(dbPath, "~/") {
				return "", fmt.Errorf("unsupported database path %q", dbPath)
			}
			dbPath = filepath.Join(home, dbPath[2:])
		}
	}

	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("database file not found: %s", abs)
		}
		return "", fmt.Errorf("database file %s: %w", abs, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("database path is a directory: %s", abs)
	}

	return abs, nil
}

func quoteIdent(s string) string {
	escaped := strings.ReplaceAll(s, `"`, `""`)
	return `"` + escaped + `"`
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage:
  renamer-go preview --db <db.sqlite> --csv <output.csv> [options]
  renamer-go list    --db <db.sqlite> [options]
  renamer-go apply   --db <db.sqlite> [--folder <path>] [--dry-run] [options]

Options:
  --folder "/Stars/Album"   Only files in this folder (relative to /Merge)
  --pattern "{meta:artist} - {meta:title}{ext}"
  --files-table files
  --folders-table folders
  --metadata-table file_metadata
  --id-column id
  --name-column basename
  --parent-folder-id-column parent_folder_id
  --folder-id-column id
  --folder-path-column path
  --meta-file-id-column file_id
  --meta-key-column key
  --meta-value-column value
`)
}

func exitWithError(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
