package a2a

import a2atype "github.com/a2aproject/a2a-go/v2/a2a"

const (
	// WorkspaceFilePartType marks a raw part carrying a file the agent wrote
	// in its workspace; the part's filename is the workspace-relative path.
	WorkspaceFilePartType = "file"
	// WorkspaceFileTooLargePartType marks a data part naming a workspace file
	// too large to send in one message.
	WorkspaceFileTooLargePartType = "file_too_large"

	ContentEncodingGzip = "gzip"
)

// NewWorkspaceFilePart returns a workspace file's bytes, as encoded by
// encoding ("" or ContentEncodingGzip).
func NewWorkspaceFilePart(path, mediaType string, content []byte, encoding string) *a2atype.Part {
	part := a2atype.NewRawPart(content)
	part.Filename = path
	part.MediaType = mediaType
	part.SetMeta(PartTypeMetadataKey, WorkspaceFilePartType)
	if encoding != "" {
		part.SetMeta(ContentEncodingMetadataKey, encoding)
	}
	return part
}

// NewWorkspaceFileTooLargePart names a workspace file without its bytes.
func NewWorkspaceFileTooLargePart(path, mediaType string, size int64) *a2atype.Part {
	part := a2atype.NewDataPart(map[string]any{"path": path, "size": size})
	part.Filename = path
	part.MediaType = mediaType
	part.SetMeta(PartTypeMetadataKey, WorkspaceFileTooLargePartType)
	return part
}
