package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestWorkbenchArtifactTypeContract(t *testing.T) {
	for _, tc := range []struct{ name, kind, format, mime string }{
		{"DECK.PPTX", "presentation", "pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"},
		{"legacy.ppt", "presentation", "unsupported", "application/vnd.ms-powerpoint"},
		{"page.html", "webpage", "html", "text/html"},
		{"data.xlsx", "spreadsheet", "spreadsheet", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{"data.csv", "spreadsheet", "spreadsheet", "text/csv"},
		{"data.tsv", "spreadsheet", "spreadsheet", "text/tab-separated-values"},
		{"notes.md", "file", "text", "text/markdown"},
		{"readme.pdf", "file", "pdf", "application/pdf"},
		{"image.png", "file", "image", "image/png"},
		{"data.bin", "file", "unsupported", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, _, _, _ := newWorkbenchForTest(sandbox.SandboxTypeDocker)
			store.entries = []sandbox.RemoteDirEntry{{Name: tc.name, Path: sandbox.SessionOutputRoot + "/" + tc.name, Type: sandbox.RemoteEntryFile}}
			files, err := svc.ListFiles(context.Background(), "s-1", "")
			require.NoError(t, err)
			data, err := json.Marshal(files)
			require.NoError(t, err)
			var rows []map[string]any
			require.NoError(t, json.Unmarshal(data, &rows))
			require.Len(t, rows, 1)
			require.Equal(t, tc.kind, rows[0]["artifact_type"])
			require.Equal(t, tc.format, rows[0]["preview_format"])
			require.Equal(t, tc.mime, rows[0]["media_type"])
			require.Equal(t, "extension", rows[0]["classification_source"], "inference must not be presented as an explicit producer declaration")
		})
	}
}

func TestWorkbenchArtifactDirectoryHasNoPreviewType(t *testing.T) {
	svc, store, _, _, _ := newWorkbenchForTest(sandbox.SandboxTypeDocker)
	store.entries = []sandbox.RemoteDirEntry{{Name: "folder.html", Path: sandbox.SessionOutputRoot + "/folder.html", Type: sandbox.RemoteEntryDir}}
	files, err := svc.ListFiles(context.Background(), "s-1", "")
	require.NoError(t, err)
	data, err := json.Marshal(files)
	require.NoError(t, err)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(data, &rows))
	require.Empty(t, rows[0]["artifact_type"])
	require.Empty(t, rows[0]["preview_format"])
}
