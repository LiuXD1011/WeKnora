package service

import (
	"path"
	"strings"

	"github.com/Tencent/WeKnora/internal/sandbox"
)

// classifyWorkbenchArtifact supplies a stable backend contract. It is an
// extension-based hint, not content validation or producer-declared metadata.
// Preview permissions still come from the renderer's fixed isolation policy.
func classifyWorkbenchArtifact(file *SandboxWorkbenchFile) {
	if file.Type != sandbox.RemoteEntryFile {
		return
	}
	file.ArtifactType, file.PreviewFormat, file.MediaType = "file", "unsupported", "application/octet-stream"
	file.ClassificationSource = "extension"
	switch strings.ToLower(path.Ext(file.Name)) {
	case ".pptx":
		file.ArtifactType, file.PreviewFormat, file.MediaType = "presentation", "pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".ppt":
		// The browser renderer understands OOXML, not legacy binary PPT.
		file.ArtifactType, file.MediaType = "presentation", "application/vnd.ms-powerpoint"
	case ".html", ".htm":
		file.ArtifactType, file.PreviewFormat, file.MediaType = "webpage", "html", "text/html"
	case ".xlsx":
		file.ArtifactType, file.PreviewFormat, file.MediaType = "spreadsheet", "spreadsheet", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".xls":
		file.ArtifactType, file.PreviewFormat, file.MediaType = "spreadsheet", "spreadsheet", "application/vnd.ms-excel"
	case ".csv":
		file.ArtifactType, file.PreviewFormat, file.MediaType = "spreadsheet", "spreadsheet", "text/csv"
	case ".tsv":
		file.ArtifactType, file.PreviewFormat, file.MediaType = "spreadsheet", "spreadsheet", "text/tab-separated-values"
	case ".pdf":
		file.PreviewFormat, file.MediaType = "pdf", "application/pdf"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg":
		file.PreviewFormat = "image"
		file.MediaType = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml"}[strings.ToLower(path.Ext(file.Name))]
	case ".txt", ".log", ".md", ".json", ".yaml", ".yml":
		file.PreviewFormat, file.MediaType = "text", "text/plain"
		if strings.EqualFold(path.Ext(file.Name), ".md") {
			file.MediaType = "text/markdown"
		}
		if strings.EqualFold(path.Ext(file.Name), ".json") {
			file.MediaType = "application/json"
		}
	}
}
