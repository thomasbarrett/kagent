package a2a

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

func TestUploadName(t *testing.T) {
	for _, tc := range []struct {
		filename string
		want     string
		wantErr  bool
	}{
		{filename: "site.kml", want: "site.kml"},
		{filename: "../../etc/passwd", want: "passwd"},
		{filename: `C:\Users\civil\site plan.dwg`, want: "site plan.dwg"},
		{filename: "/abs/path/boundary.geojson", want: "boundary.geojson"},
		{filename: "", wantErr: true},
		{filename: "..", wantErr: true},
		{filename: "dir/", want: "dir"},
		{filename: "/", wantErr: true},
		{filename: ".bashrc", wantErr: true},
		{filename: "bad\x00name", wantErr: true},
		{filename: "bad\nname", wantErr: true},
		{filename: "\xff.kml", wantErr: true},
		{filename: strings.Repeat("a", maxFilenameBytes+1), wantErr: true},
	} {
		t.Run(tc.filename, func(t *testing.T) {
			got, err := uploadName(tc.filename)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("uploadName(%q) = %q, want error", tc.filename, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("uploadName(%q) = %q, %v; want %q", tc.filename, got, err, tc.want)
			}
		})
	}
}

func TestParseUpload(t *testing.T) {
	t.Run("raw bytes", func(t *testing.T) {
		part := fileUploadPart("site.kml", []byte("<kml/>"), "")
		got, err := parseUpload(part)
		if err != nil || got.name != "site.kml" || string(got.content) != "<kml/>" || got.encoding != "" {
			t.Fatalf("parseUpload() = %#v, %v", got, err)
		}
	})
	t.Run("gzip", func(t *testing.T) {
		got, err := parseUpload(fileUploadPart("site.kml", gzipped(t, "<kml/>"), apia2a.ContentEncodingGzip))
		if err != nil || got.encoding != apia2a.ContentEncodingGzip {
			t.Fatalf("parseUpload() = %#v, %v", got, err)
		}
	})
	for name, part := range map[string]*a2atype.Part{
		"url":              {Content: a2atype.URL("https://example.com/site.kml"), Filename: "site.kml"},
		"data":             {Content: a2atype.Data{Value: map[string]any{"a": 1}}},
		"unknown encoding": fileUploadPart("site.kml", []byte("x"), "br"),
		"no filename":      fileUploadPart("", []byte("x"), ""),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseUpload(part); err == nil {
				t.Fatal("parseUpload() succeeded, want error")
			}
		})
	}
}

func TestSaveUploads(t *testing.T) {
	t.Run("writes plain and gzip files and replaces by name", func(t *testing.T) {
		workspace := t.TempDir()
		if _, err := saveUploads(workspace, []upload{{name: "site.kml", content: []byte("old")}}); err != nil {
			t.Fatal(err)
		}
		saved, err := saveUploads(workspace, []upload{
			{name: "site.kml", content: gzipped(t, "<kml>new</kml>"), encoding: apia2a.ContentEncodingGzip},
			{name: "notes.txt", content: []byte("hi")},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []savedUpload{{path: "uploads/site.kml", size: 14}, {path: "uploads/notes.txt", size: 2}}
		if !slices.Equal(saved, want) {
			t.Errorf("saved = %#v, want %#v", saved, want)
		}
		assertFile(t, filepath.Join(workspace, "uploads", "site.kml"), "<kml>new</kml>")
		assertFile(t, filepath.Join(workspace, "uploads", "notes.txt"), "hi")
		entries, _ := os.ReadDir(filepath.Join(workspace, "uploads"))
		if len(entries) != 2 {
			t.Errorf("uploads holds %d entries, want no temporary files left", len(entries))
		}
	})
	t.Run("invalid gzip leaves nothing behind", func(t *testing.T) {
		workspace := t.TempDir()
		if _, err := saveUploads(workspace, []upload{{name: "a.kml", content: []byte("not gzip"), encoding: apia2a.ContentEncodingGzip}}); err == nil {
			t.Fatal("saveUploads() succeeded, want error")
		}
		entries, _ := os.ReadDir(filepath.Join(workspace, "uploads"))
		if len(entries) != 0 {
			t.Errorf("uploads holds %d entries, want none", len(entries))
		}
	})
	t.Run("decoded size is bounded", func(t *testing.T) {
		dir := t.TempDir()
		bomb := gzipped(t, strings.Repeat("0", 4096))
		if _, err := writeUpload(dir, upload{name: "a.txt", content: bomb, encoding: apia2a.ContentEncodingGzip}, 1024); err == nil {
			t.Fatal("writeUpload() succeeded past its budget")
		}
		if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
			t.Errorf("oversized upload was written: %v", err)
		}
	})
}

func TestUploadNote(t *testing.T) {
	got := uploadNote([]savedUpload{{path: "uploads/site.kml", size: 42 << 20}, {path: "uploads/a.txt", size: 12}})
	want := "The user attached these files, saved in the workspace:\n- uploads/site.kml (42.0 MiB)\n- uploads/a.txt (12 bytes)"
	if got != want {
		t.Errorf("uploadNote() = %q, want %q", got, want)
	}
}

func TestWorkspaceFilesChanged(t *testing.T) {
	t.Run("reports new and modified files once", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "uploads/site.kml", "upload")
		writeFile(t, root, "kept.txt", "same")
		files, err := newWorkspaceFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "out/boundary.geojson", "{}")
		writeFile(t, root, "kept.txt", "changed size")
		writeFile(t, root, ".hidden", "x")
		writeFile(t, root, ".git/HEAD", "x")
		writeFile(t, root, "node_modules/pkg/index.js", "x")
		writeFile(t, root, "py/__pycache__/m.pyc", "x")
		if err := os.Symlink(filepath.Join(root, "kept.txt"), filepath.Join(root, "link.txt")); err != nil {
			t.Fatal(err)
		}
		assertChanged(t, files, []string{"kept.txt", "out/boundary.geojson"})
		assertChanged(t, files, nil)

		// The same size still counts when the modification time moves.
		later := time.Now().Add(time.Minute)
		if err := os.Chtimes(filepath.Join(root, "kept.txt"), later, later); err != nil {
			t.Fatal(err)
		}
		assertChanged(t, files, []string{"kept.txt"})
	})
	t.Run("caps each scan and defers the rest", func(t *testing.T) {
		root := t.TempDir()
		files, err := newWorkspaceFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		for i := range maxFilesPerScan + 3 {
			writeFile(t, root, filepath.Join("out", string(rune('a'+i))+".txt"), "x")
		}
		first, err := files.changed()
		if err != nil || len(first) != maxFilesPerScan {
			t.Fatalf("first scan = %d files, %v", len(first), err)
		}
		second, err := files.changed()
		if err != nil || len(second) != 3 {
			t.Fatalf("second scan = %d files, %v", len(second), err)
		}
	})
	t.Run("missing workspace is empty", func(t *testing.T) {
		files, err := newWorkspaceFiles(filepath.Join(t.TempDir(), "absent"))
		if err != nil {
			t.Fatal(err)
		}
		assertChanged(t, files, nil)
	})
}

func TestWorkspaceFilePart(t *testing.T) {
	root := t.TempDir()
	t.Run("small file is sent as is", func(t *testing.T) {
		writeFile(t, root, "a.txt", "hello")
		part := mustFilePart(t, root, "a.txt")
		assertFilePart(t, part, "a.txt", "text/plain", "", "hello")
	})
	t.Run("compressible file is gzipped", func(t *testing.T) {
		content := strings.Repeat("<Placemark><name>pad</name></Placemark>\n", 2000)
		writeFile(t, root, "out/site.kml", content)
		part := mustFilePart(t, root, "out/site.kml")
		assertFilePart(t, part, "out/site.kml", "application/vnd.google-earth.kml+xml", apia2a.ContentEncodingGzip, content)
	})
	t.Run("compressed format is never gzipped", func(t *testing.T) {
		content := strings.Repeat("a", 4096)
		writeFile(t, root, "plot.png", content)
		assertFilePart(t, mustFilePart(t, root, "plot.png"), "plot.png", "image/png", "", content)
	})
	t.Run("incompressible file over the limit is named only", func(t *testing.T) {
		noise := make([]byte, maxOutputBytes+1)
		if _, err := rand.Read(noise); err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "noise.bin", string(noise))
		assertTooLarge(t, mustFilePart(t, root, "noise.bin"), "noise.bin", int64(len(noise)))
	})
	t.Run("compressed format over the limit is not read", func(t *testing.T) {
		name := filepath.Join(root, "big.kmz")
		file, err := os.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxOutputBytes + 1); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		assertTooLarge(t, mustFilePart(t, root, "big.kmz"), "big.kmz", maxOutputBytes+1)
	})
}

func TestMediaTypeOf(t *testing.T) {
	for path, want := range map[string]string{
		"a/site.KML":       "application/vnd.google-earth.kml+xml",
		"site.kmz":         "application/vnd.google-earth.kmz",
		"b.geojson":        "application/geo+json",
		"plan.dxf":         "image/vnd.dxf",
		"plan.dwg":         "image/vnd.dwg",
		"report.pdf":       "application/pdf",
		"no-extension":     "application/octet-stream",
		"strange.whatever": "application/octet-stream",
	} {
		if got := mediaTypeOf(path); got != want {
			t.Errorf("mediaTypeOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestExecuteExchangesWorkspaceFiles(t *testing.T) {
	workspace := t.TempDir()
	var prompt string
	executor, err := New(fakeRunner{run: func(_ context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
		prompt = turn.Prompt
		upload, err := os.ReadFile(filepath.Join(workspace, "uploads", "site.kml"))
		if err != nil || string(upload) != "<kml/>" {
			t.Errorf("upload on disk = %q, %v", upload, err)
		}
		writeFile(t, workspace, "out/buffer.geojson", "{}")
		if err := sink.ToolCall(runtime.ToolCall{ID: "tool-1", Name: "Bash"}); err != nil {
			return runtime.Outcome{}, err
		}
		if err := sink.ToolResult(runtime.ToolResult{ID: "tool-1", Name: "Bash", Result: "ok"}); err != nil {
			return runtime.Outcome{}, err
		}
		// Written after the last tool result, so only the closing scan sees it.
		writeFile(t, workspace, "report.md", "# Report")
		return runtime.Outcome{}, sink.TextDelta(runtime.TextDelta{Text: "done"})
	}}, &fakeContinuation{}, tracing.RuntimeTelemetry{}, WithWorkspace(workspace))
	if err != nil {
		t.Fatal(err)
	}
	reqCtx := requestContext("task-files", "Buffer this site")
	reqCtx.Message.Parts = append(reqCtx.Message.Parts, fileUploadPart("site.kml", gzipped(t, "<kml/>"), apia2a.ContentEncodingGzip))

	events, errs := collect(executor.Execute(t.Context(), reqCtx))
	if len(errs) != 0 {
		t.Fatalf("Execute() errors = %v", errs)
	}
	wantPrompt := "Buffer this site\n\nThe user attached these files, saved in the workspace:\n- uploads/site.kml (6 bytes)"
	if prompt != wantPrompt {
		t.Errorf("prompt = %q, want %q", prompt, wantPrompt)
	}
	var order []string
	for _, event := range events {
		switch event := event.(type) {
		case *a2atype.TaskArtifactUpdateEvent:
			part := event.Artifact.Parts[0]
			if part.Metadata[apia2a.PartTypeMetadataKey] == apia2a.WorkspaceFilePartType {
				if event.Artifact.Name != part.Filename || !event.LastChunk {
					t.Errorf("file artifact %q: name %q, last chunk %v", part.Filename, event.Artifact.Name, event.LastChunk)
				}
				if _, ok := apia2a.TimelinePosition(event.Artifact); !ok {
					t.Errorf("file artifact %q has no timeline position", part.Filename)
				}
				order = append(order, "file:"+part.Filename)
			} else if kind, ok := part.Metadata[apia2a.PartTypeMetadataKey].(string); ok {
				order = append(order, kind)
			} else {
				order = append(order, "text")
			}
		case *a2atype.TaskStatusUpdateEvent:
			order = append(order, string(event.Status.State))
		}
	}
	want := []string{
		string(a2atype.TaskStateWorking), "function_call", "function_response", "file:out/buffer.geojson",
		"text", "file:report.md", string(a2atype.TaskStateCompleted),
	}
	if !slices.Equal(order, want) {
		t.Errorf("events = %v, want %v", order, want)
	}
}

func TestExecuteRejectsUnsupportedParts(t *testing.T) {
	runner := fakeRunner{run: func(context.Context, runtime.Turn, runtime.EventSink) (runtime.Outcome, error) {
		t.Error("runner ran for an invalid request")
		return runtime.Outcome{}, nil
	}}
	withoutWorkspace, err := New(runner, &fakeContinuation{}, tracing.RuntimeTelemetry{})
	if err != nil {
		t.Fatal(err)
	}
	withWorkspace, err := New(runner, &fakeContinuation{}, tracing.RuntimeTelemetry{}, WithWorkspace(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		executor *Executor
		parts    []*a2atype.Part
	}{
		{name: "file without a workspace", executor: withoutWorkspace, parts: []*a2atype.Part{a2atype.NewTextPart("hi"), fileUploadPart("a.txt", []byte("x"), "")}},
		{name: "two text parts", executor: withWorkspace, parts: []*a2atype.Part{a2atype.NewTextPart("a"), a2atype.NewTextPart("b")}},
		{name: "empty text", executor: withWorkspace, parts: []*a2atype.Part{a2atype.NewTextPart("")}},
		{name: "duplicate names", executor: withWorkspace, parts: []*a2atype.Part{fileUploadPart("a.txt", []byte("1"), ""), fileUploadPart("dir/a.txt", []byte("2"), "")}},
		{name: "file URL", executor: withWorkspace, parts: []*a2atype.Part{a2atype.NewFileURLPart("https://example.com/a.kml", "")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := a2atype.NewMessage(a2atype.MessageRoleUser, tc.parts...)
			message.TaskID, message.ContextID = "task-invalid", testContextID
			_, errs := collect(tc.executor.Execute(t.Context(), &a2asrv.ExecutorContext{TaskID: "task-invalid", ContextID: testContextID, Message: message}))
			if len(errs) == 0 {
				t.Fatal("Execute() accepted the request")
			}
		})
	}
}

func TestExecuteAcceptsFilesWithoutText(t *testing.T) {
	workspace := t.TempDir()
	var prompt string
	executor, err := New(fakeRunner{run: func(_ context.Context, turn runtime.Turn, _ runtime.EventSink) (runtime.Outcome, error) {
		prompt = turn.Prompt
		return runtime.Outcome{}, nil
	}}, &fakeContinuation{}, tracing.RuntimeTelemetry{}, WithWorkspace(workspace))
	if err != nil {
		t.Fatal(err)
	}
	message := a2atype.NewMessage(a2atype.MessageRoleUser, fileUploadPart("plan.dwg", []byte("dwg"), ""))
	message.TaskID, message.ContextID = "task-file-only", testContextID
	events, errs := collect(executor.Execute(t.Context(), &a2asrv.ExecutorContext{TaskID: "task-file-only", ContextID: testContextID, Message: message}))
	if len(errs) != 0 {
		t.Fatalf("Execute() errors = %v", errs)
	}
	if want := "The user attached these files, saved in the workspace:\n- uploads/plan.dwg (3 bytes)"; prompt != want {
		t.Errorf("prompt = %q, want %q", prompt, want)
	}
	// The upload is in the baseline, so it is not handed back.
	for _, event := range events {
		if update, ok := event.(*a2atype.TaskArtifactUpdateEvent); ok {
			t.Errorf("unexpected artifact %q", update.Artifact.Parts[0].Filename)
		}
	}
}

func TestWithWorkspace(t *testing.T) {
	if _, err := New(fakeRunner{}, &fakeContinuation{}, tracing.RuntimeTelemetry{}, WithWorkspace("relative/dir")); err == nil {
		t.Fatal("New() accepted a relative workspace")
	}
}

func fileUploadPart(name string, content []byte, encoding string) *a2atype.Part {
	part := a2atype.NewRawPart(content)
	part.Filename = name
	if encoding != "" {
		part.SetMeta(apia2a.ContentEncodingMetadataKey, encoding)
	}
	return part
}

func gzipped(t *testing.T, content string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	name := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, name, want string) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || string(got) != want {
		t.Errorf("%s = %q, %v; want %q", name, got, err, want)
	}
}

func assertChanged(t *testing.T, files *workspaceFiles, want []string) {
	t.Helper()
	got, err := files.changed()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("changed() = %v, want %v", got, want)
	}
}

func mustFilePart(t *testing.T, root, rel string) *a2atype.Part {
	t.Helper()
	part, err := workspaceFilePart(root, rel)
	if err != nil {
		t.Fatal(err)
	}
	return part
}

func assertFilePart(t *testing.T, part *a2atype.Part, path, mediaType, encoding, want string) {
	t.Helper()
	if part.Filename != path || part.MediaType != mediaType || part.Metadata[apia2a.PartTypeMetadataKey] != apia2a.WorkspaceFilePartType {
		t.Fatalf("part = %q %q %v", part.Filename, part.MediaType, part.Metadata)
	}
	gotEncoding, _ := part.Metadata[apia2a.ContentEncodingMetadataKey].(string)
	if gotEncoding != encoding {
		t.Fatalf("encoding = %q, want %q", gotEncoding, encoding)
	}
	content := part.Raw()
	if encoding == apia2a.ContentEncodingGzip {
		reader, err := gzip.NewReader(bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		if content, err = io.ReadAll(reader); err != nil {
			t.Fatal(err)
		}
	}
	if string(content) != want {
		t.Errorf("content = %d bytes, want %d", len(content), len(want))
	}
}

func assertTooLarge(t *testing.T, part *a2atype.Part, path string, size int64) {
	t.Helper()
	if part.Metadata[apia2a.PartTypeMetadataKey] != apia2a.WorkspaceFileTooLargePartType || part.Filename != path {
		t.Fatalf("part = %q %v, want too-large %q", part.Filename, part.Metadata, path)
	}
	data, _ := part.Data().(map[string]any)
	if data["path"] != path || data["size"] != size {
		t.Errorf("data = %v, want path %q size %d", data, path, size)
	}
}
