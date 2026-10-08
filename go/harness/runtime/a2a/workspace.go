package a2a

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
)

// UploadsDir is the workspace directory user files land in.
const UploadsDir = "uploads"

const (
	// Gzip lets a message decode to far more than it carries, so the decoded
	// size has a bound of its own.
	maxUploadBytes   = 1 << 30
	maxFilenameBytes = 255
	// An output part shares the gRPC message limit with framing and the rest
	// of the event, so it stays well under it.
	maxOutputBytes = 12 << 20
	// A scan stops early in a workspace with a huge tree, such as a cloned
	// repository, rather than stall the turn on every tool result.
	maxScanEntries  = 10_000
	maxFilesPerScan = 20
)

// skippedDirs hold tool caches nobody asked to see.
var skippedDirs = []string{"node_modules", "__pycache__"}

// Option configures an Executor.
type Option func(*Executor) error

// WithWorkspace has the executor save user files under dir and return the
// files a turn writes there. dir is the runtime's working directory.
func WithWorkspace(dir string) Option {
	return func(e *Executor) error {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("workspace %q must be an absolute path", dir)
		}
		e.workspace = filepath.Clean(dir)
		return nil
	}
}

// upload is one user file from a request, still encoded.
type upload struct {
	name     string
	content  []byte
	encoding string
}

// savedUpload is an upload written to the workspace.
type savedUpload struct {
	path string
	size int64
}

// parseUpload validates one file part of a user message.
func parseUpload(part *a2atype.Part) (upload, error) {
	if part.URL() != "" {
		return upload{}, fmt.Errorf("harness runtime does not fetch file URLs; send the file's bytes")
	}
	if _, ok := part.Content.(a2atype.Raw); !ok {
		return upload{}, fmt.Errorf("harness runtime accepts only text and file parts")
	}
	name, err := uploadName(part.Filename)
	if err != nil {
		return upload{}, err
	}
	encoding, _ := part.Metadata[apia2a.ContentEncodingMetadataKey].(string)
	if encoding != "" && encoding != apia2a.ContentEncodingGzip {
		return upload{}, fmt.Errorf("file %q has unsupported content encoding %q", name, encoding)
	}
	return upload{name: name, content: part.Raw(), encoding: encoding}, nil
}

// uploadName reduces a client's filename to a plain name inside UploadsDir.
func uploadName(filename string) (string, error) {
	name := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	switch {
	case filename == "" || name == "." || name == ".." || name == "/":
		return "", fmt.Errorf("file part requires a filename")
	case strings.HasPrefix(name, "."):
		return "", fmt.Errorf("file %q: hidden filenames are not accepted", name)
	case len(name) > maxFilenameBytes:
		return "", fmt.Errorf("file name is longer than %d bytes", maxFilenameBytes)
	case !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl):
		return "", fmt.Errorf("file name %q contains invalid characters", name)
	}
	return name, nil
}

// saveUploads writes each upload to UploadsDir, replacing a file of the same
// name. Writes go through a temporary file so a failed decode leaves no
// partial file behind.
func saveUploads(workspace string, uploads []upload) ([]savedUpload, error) {
	if len(uploads) == 0 {
		return nil, nil
	}
	dir := filepath.Join(workspace, UploadsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create uploads directory: %w", err)
	}
	saved := make([]savedUpload, 0, len(uploads))
	budget := int64(maxUploadBytes)
	for _, file := range uploads {
		size, err := writeUpload(dir, file, budget)
		if err != nil {
			return nil, err
		}
		budget -= size
		saved = append(saved, savedUpload{path: UploadsDir + "/" + file.name, size: size})
	}
	return saved, nil
}

func writeUpload(dir string, file upload, budget int64) (int64, error) {
	var content io.Reader = bytes.NewReader(file.content)
	if file.encoding == apia2a.ContentEncodingGzip {
		decoded, err := gzip.NewReader(content)
		if err != nil {
			return 0, fmt.Errorf("file %q is not valid gzip: %w", file.name, err)
		}
		defer decoded.Close()
		content = decoded
	}
	temp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return 0, fmt.Errorf("save file %q: %w", file.name, err)
	}
	defer os.Remove(temp.Name())
	// One byte past the budget tells an oversized file from one that fits exactly.
	size, err := io.Copy(temp, io.LimitReader(content, budget+1))
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, fmt.Errorf("save file %q: %w", file.name, err)
	}
	if size > budget {
		return 0, fmt.Errorf("files in one message exceed %d MiB once decoded", maxUploadBytes>>20)
	}
	if err := os.Chmod(temp.Name(), 0o644); err != nil {
		return 0, fmt.Errorf("save file %q: %w", file.name, err)
	}
	if err := os.Rename(temp.Name(), filepath.Join(dir, file.name)); err != nil {
		return 0, fmt.Errorf("save file %q: %w", file.name, err)
	}
	return size, nil
}

// uploadNote tells the model where the user's files are, so it opens them
// with its own tools rather than reading their bytes in the prompt.
func uploadNote(saved []savedUpload) string {
	var note strings.Builder
	note.WriteString("The user attached these files, saved in the workspace:")
	for _, file := range saved {
		fmt.Fprintf(&note, "\n- %s (%s)", file.path, formatSize(file.size))
	}
	return note.String()
}

func formatSize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

type fileState struct {
	size    int64
	modTime time.Time
}

// workspaceFiles reports the files a turn writes, by comparing each scan with
// the one before.
type workspaceFiles struct {
	root     string
	baseline map[string]fileState
}

func newWorkspaceFiles(root string) (*workspaceFiles, error) {
	current, err := scanWorkspace(root)
	if err != nil {
		return nil, err
	}
	return &workspaceFiles{root: root, baseline: current}, nil
}

// changed returns the new or modified files since the last call, in path
// order. Past maxFilesPerScan the rest wait for the next call.
func (w *workspaceFiles) changed() ([]string, error) {
	current, err := scanWorkspace(w.root)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rel, state := range current {
		if previous, ok := w.baseline[rel]; !ok || previous != state {
			paths = append(paths, rel)
		}
	}
	slices.Sort(paths)
	if len(paths) > maxFilesPerScan {
		paths = paths[:maxFilesPerScan]
	}
	for _, rel := range paths {
		w.baseline[rel] = current[rel]
	}
	return paths, nil
}

// scanWorkspace lists regular files under root by workspace-relative,
// slash-separated path. A missing root is an empty workspace.
func scanWorkspace(root string) (map[string]fileState, error) {
	files := map[string]fileState{}
	entries := 0
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			if name == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			// An entry removed mid-walk is no longer an output.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if name == root {
			return nil
		}
		if entries++; entries > maxScanEntries {
			return fs.SkipAll
		}
		base := entry.Name()
		if entry.IsDir() {
			if strings.HasPrefix(base, ".") || slices.Contains(skippedDirs, base) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(base, ".") || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = fileState{size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan workspace: %w", err)
	}
	return files, nil
}

// workspaceFilePart reads one workspace file into a part, gzipped when that
// saves enough to matter, or names it when it cannot fit in a message.
func workspaceFilePart(root, rel string) (*a2atype.Part, error) {
	mediaType := mediaTypeOf(rel)
	file, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if !compresses(rel) {
		if size > maxOutputBytes {
			return apia2a.NewWorkspaceFileTooLargePart(rel, mediaType, size), nil
		}
		content, err := io.ReadAll(io.LimitReader(file, maxOutputBytes+1))
		if err != nil {
			return nil, err
		}
		if len(content) > maxOutputBytes {
			return apia2a.NewWorkspaceFileTooLargePart(rel, mediaType, int64(len(content))), nil
		}
		return apia2a.NewWorkspaceFilePart(rel, mediaType, content, ""), nil
	}
	compressed, read, fits, err := gzipBounded(file, maxOutputBytes)
	if err != nil {
		return nil, err
	}
	if !fits {
		return apia2a.NewWorkspaceFileTooLargePart(rel, mediaType, max(size, read)), nil
	}
	// Gzip that saves little only costs the reader a decode.
	if read <= maxOutputBytes && int64(len(compressed)) > read*9/10 {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		content, err := io.ReadAll(io.LimitReader(file, read))
		if err != nil {
			return nil, err
		}
		return apia2a.NewWorkspaceFilePart(rel, mediaType, content, ""), nil
	}
	return apia2a.NewWorkspaceFilePart(rel, mediaType, compressed, apia2a.ContentEncodingGzip), nil
}

// gzipBounded compresses src while the output stays within limit, so memory
// stays bounded whatever the file's size. It reports the bytes read and
// whether the whole file fit.
func gzipBounded(src io.Reader, limit int) ([]byte, int64, bool, error) {
	out := &boundedBuffer{limit: limit}
	writer := gzip.NewWriter(out)
	read, err := io.Copy(writer, src)
	if err == nil {
		err = writer.Close()
	}
	if errors.Is(err, errBufferFull) {
		return nil, read, false, nil
	}
	if err != nil {
		return nil, read, false, err
	}
	return out.Bytes(), read, true, nil
}

var errBufferFull = errors.New("buffer limit reached")

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errBufferFull
	}
	return b.Buffer.Write(p)
}

// Formats that are already compressed, so gzip would only cost time.
var compressedExtensions = []string{
	".kmz", ".zip", ".gz", ".tgz", ".bz2", ".xz", ".zst", ".7z",
	".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".heic",
	".mp3", ".mp4", ".mov", ".webm", ".docx", ".xlsx", ".pptx",
	".parquet", ".pmtiles", ".tif", ".tiff",
}

func compresses(rel string) bool {
	return !slices.Contains(compressedExtensions, strings.ToLower(path.Ext(rel)))
}

var mediaTypes = map[string]string{
	".kml":     "application/vnd.google-earth.kml+xml",
	".kmz":     "application/vnd.google-earth.kmz",
	".geojson": "application/geo+json",
	".json":    "application/json",
	".csv":     "text/csv",
	".tsv":     "text/tab-separated-values",
	".txt":     "text/plain",
	".md":      "text/markdown",
	".html":    "text/html",
	".xml":     "application/xml",
	".yaml":    "application/yaml",
	".yml":     "application/yaml",
	".pdf":     "application/pdf",
	".png":     "image/png",
	".jpg":     "image/jpeg",
	".jpeg":    "image/jpeg",
	".gif":     "image/gif",
	".webp":    "image/webp",
	".svg":     "image/svg+xml",
	".dxf":     "image/vnd.dxf",
	".dwg":     "image/vnd.dwg",
	".zip":     "application/zip",
	".gz":      "application/gzip",
	".tif":     "image/tiff",
	".tiff":    "image/tiff",
	".shp":     "application/vnd.shp",
	".gpkg":    "application/geopackage+sqlite3",
	".parquet": "application/vnd.apache.parquet",
}

func mediaTypeOf(rel string) string {
	if mediaType, ok := mediaTypes[strings.ToLower(path.Ext(rel))]; ok {
		return mediaType
	}
	return "application/octet-stream"
}
