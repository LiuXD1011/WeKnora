package service

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/Tencent/WeKnora/internal/sandbox"
)

const workbenchArtifactManifestName = ".weknora-artifacts.json"

type workbenchArtifactDeclaration struct {
	Path          string `json:"path"`
	ArtifactType  string `json:"artifact_type"`
	PreviewFormat string `json:"preview_format"`
	MediaType     string `json:"media_type"`
}

type workbenchArtifactManifest struct {
	Version   int                            `json:"version"`
	Artifacts []workbenchArtifactDeclaration `json:"artifacts"`
}

func parseWorkbenchArtifactManifest(data []byte) map[string]workbenchArtifactDeclaration {
	if len(data) == 0 || len(data) > 1<<20 {
		return nil
	}
	var manifest workbenchArtifactManifest
	if json.Unmarshal(data, &manifest) != nil || manifest.Version != 1 {
		return nil
	}
	result := make(map[string]workbenchArtifactDeclaration, len(manifest.Artifacts))
	for _, item := range manifest.Artifacts {
		if path.Clean(item.Path) != item.Path || strings.Contains(item.Path, "/") || item.Path == workbenchArtifactManifestName {
			continue
		}
		if !validWorkbenchArtifactDeclaration(item) {
			continue
		}
		result[item.Path] = item
	}
	return result
}

func validWorkbenchArtifactDeclaration(item workbenchArtifactDeclaration) bool {
	allowed := map[string]map[string]bool{
		"presentation": {"pptx": true},
		"webpage":      {"html": true},
		"spreadsheet":  {"spreadsheet": true},
		"file":         {"pdf": true, "image": true, "text": true, "unsupported": true},
	}
	formats, ok := allowed[item.ArtifactType]
	return ok && formats[item.PreviewFormat] && strings.TrimSpace(item.MediaType) != "" && !strings.ContainsAny(item.MediaType, "\r\n")
}

func applyWorkbenchArtifactDeclaration(file *SandboxWorkbenchFile, item workbenchArtifactDeclaration) {
	if file.Type != sandbox.RemoteEntryFile {
		return
	}
	file.ArtifactType = item.ArtifactType
	file.PreviewFormat = item.PreviewFormat
	file.MediaType = item.MediaType
	file.ClassificationSource = "producer"
}

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
