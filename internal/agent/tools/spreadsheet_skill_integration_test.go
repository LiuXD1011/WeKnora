//go:build spreadsheet_integration

package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestSpreadsheetGeneratorRealSkillChain(t *testing.T) {
	image := strings.TrimSpace(os.Getenv("SPREADSHEET_INTEGRATION_IMAGE"))
	require.NotEmpty(t, image, "set SPREADSHEET_INTEGRATION_IMAGE")
	evidenceDir := strings.TrimSpace(os.Getenv("SPREADSHEET_EVIDENCE_DIR"))
	require.NotEmpty(t, evidenceDir, "set SPREADSHEET_EVIDENCE_DIR")

	cfg := sandbox.DefaultConfig()
	cfg.Type = sandbox.SandboxTypeDocker
	cfg.DockerHost = "unix:///var/run/docker.sock"
	cfg.DockerImage = image
	cfg.DefaultTimeout = 2 * time.Minute
	remote, err := sandbox.NewDockerRemoteClient(cfg)
	require.NoError(t, err)
	manager, err := sandbox.NewSessionBoundManager(sandbox.SessionBoundManagerConfig{
		Config: cfg, Client: remote,
		Store:    sandbox.NewMemorySessionSandboxBindingStore(),
		Checker:  sandbox.PermissiveSessionExistenceChecker{},
		ConfigID: "spreadsheet-integration", SkipHealthProbe: true,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ctx = types.WithSandboxTenantID(ctx, 1002)
	ctx = types.WithSessionID(ctx, "spreadsheet-chain")
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(types.WithSandboxTenantID(context.Background(), 1002), 30*time.Second)
		defer stop()
		require.NoError(t, manager.DestroySession(cleanup, "spreadsheet-chain"))
	})

	preloadedRoot := strings.TrimSpace(os.Getenv("SPREADSHEET_SKILL_DIR"))
	if preloadedRoot == "" {
		preloadedRoot = filepath.Join("skills", "preloaded")
	}
	preloaded, err := filepath.Abs(preloadedRoot)
	require.NoError(t, err)
	skillManager := skills.NewManager(&skills.ManagerConfig{
		SkillDirs: []string{preloaded}, AllowedSkills: []string{"数据处理器"}, Enabled: true,
	}, manager)
	require.NoError(t, skillManager.Initialize(ctx))
	require.Len(t, skillManager.GetAllMetadata(), 1)

	payload := map[string]any{
		"output_name": "weknora-sandbox-acceptance",
		"title":       "WeKnora 沙盒验收表",
		"sheet_name":  "验收结果",
		"columns":     []string{"验收项", "后端", "结果", "说明"},
		"rows": [][]any{
			{"演示文稿预览", "Docker", "通过", "PPTX 与 HTML 可预览下载"},
			{"电子表格预览", "Docker", "通过", "XLSX 可内嵌预览下载"},
			{"租户隔离", "Docker", "通过", 2},
		},
	}
	payloadBytes, err := json.Marshal(payload)
	require.NoError(t, err)
	args, err := json.Marshal(ExecuteSkillScriptInput{
		SkillName: "数据处理器", ScriptPath: "scripts/generate_spreadsheet.py", Input: string(payloadBytes),
	})
	require.NoError(t, err)
	result, err := NewExecuteSkillScriptTool(skillManager).Execute(ctx, args)
	require.NoError(t, err)
	require.True(t, result.Success, result.Output+"\n"+result.Error)
	stdout, ok := result.Data["stdout"].(string)
	require.True(t, ok)
	var declared struct {
		Success   bool `json:"success"`
		Artifacts []struct {
			Path          string `json:"path"`
			Type          string `json:"type"`
			ArtifactType  string `json:"artifact_type"`
			PreviewFormat string `json:"preview_format"`
			MediaType     string `json:"media_type"`
		} `json:"artifacts"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &declared), stdout)
	require.True(t, declared.Success)
	require.Len(t, declared.Artifacts, 1)
	require.Equal(t, "spreadsheet", declared.Artifacts[0].Type)
	require.Equal(t, "spreadsheet", declared.Artifacts[0].ArtifactType)
	require.Equal(t, "spreadsheet", declared.Artifacts[0].PreviewFormat)
	require.NotEmpty(t, declared.Artifacts[0].MediaType)

	store := manager.SessionFileStore()
	require.NotNil(t, store)
	outputDir := skillManager.SkillOutputDir("spreadsheet-chain", "数据处理器")
	entries, err := store.ListSessionFiles(ctx, "spreadsheet-chain", outputDir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "XLSX and producer manifest must be emitted")
	require.NoError(t, os.MkdirAll(evidenceDir, 0o755))
	for _, name := range []string{"weknora-sandbox-acceptance.xlsx", ".weknora-artifacts.json"} {
		data, err := store.ReadSessionFile(ctx, "spreadsheet-chain", outputDir+"/"+name)
		require.NoError(t, err)
		require.NotEmpty(t, data)
		require.NoError(t, os.WriteFile(filepath.Join(evidenceDir, name), data, 0o644))
		if strings.HasSuffix(name, ".xlsx") {
			validateSpreadsheetOOXML(t, data)
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(evidenceDir, "tool-result.json"), []byte(fmt.Sprintf("%s\n", stdout)), 0o644))
}

func validateSpreadsheetOOXML(t *testing.T, data []byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	files := make(map[string]*zip.File, len(zr.File))
	for _, file := range zr.File {
		files[file.Name] = file
	}
	for _, name := range []string{"[Content_Types].xml", "xl/workbook.xml", "xl/styles.xml", "xl/worksheets/sheet1.xml"} {
		require.Contains(t, files, name)
	}
	reader, err := files["xl/worksheets/sheet1.xml"].Open()
	require.NoError(t, err)
	defer reader.Close()
	var sheet bytes.Buffer
	_, err = sheet.ReadFrom(reader)
	require.NoError(t, err)
	require.Contains(t, sheet.String(), "WeKnora 沙盒验收表")
	require.Contains(t, sheet.String(), "电子表格预览")
	require.Contains(t, sheet.String(), "<autoFilter")
	require.Contains(t, sheet.String(), "state=\"frozen\"")
}
