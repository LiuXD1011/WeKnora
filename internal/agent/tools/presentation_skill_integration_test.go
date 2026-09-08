//go:build presentation_integration

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

func TestPresentationGeneratorRealSkillChain(t *testing.T) {
	image := strings.TrimSpace(os.Getenv("PRESENTATION_INTEGRATION_IMAGE"))
	require.NotEmpty(t, image, "set PRESENTATION_INTEGRATION_IMAGE")
	evidenceDir := strings.TrimSpace(os.Getenv("PRESENTATION_EVIDENCE_DIR"))
	require.NotEmpty(t, evidenceDir, "set PRESENTATION_EVIDENCE_DIR")

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
		ConfigID: "presentation-integration", SkipHealthProbe: true,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	ctx = types.WithSandboxTenantID(ctx, 1001)
	ctx = types.WithSessionID(ctx, "presentation-chain")
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(types.WithSandboxTenantID(context.Background(), 1001), 30*time.Second)
		defer stop()
		require.NoError(t, manager.DestroySession(cleanup, "presentation-chain"))
	})

	preloadedRoot := strings.TrimSpace(os.Getenv("PRESENTATION_SKILL_DIR"))
	if preloadedRoot == "" {
		preloadedRoot = filepath.Join("skills", "preloaded")
	}
	preloaded, err := filepath.Abs(preloadedRoot)
	require.NoError(t, err)
	skillManager := skills.NewManager(&skills.ManagerConfig{
		SkillDirs: []string{preloaded}, AllowedSkills: []string{"演示文稿生成器"}, Enabled: true,
	}, manager)
	require.NoError(t, skillManager.Initialize(ctx))
	require.Len(t, skillManager.GetAllMetadata(), 1)

	payload := map[string]any{
		"title": "WeKnora 沙盒工作台实战", "subtitle": "真实 Skill 链路验收", "author": "刘雪灯",
		"output_name": "weknora-sandbox-demo",
		"slides": []map[string]any{
			{"title": "统一工作台", "bullets": []string{"终端与产物集中展示", "Docker 与第二后端使用统一接口"}},
			{"title": "隔离与资源控制", "bullets": []string{"租户与会话双重隔离", "超限自动终止并记录审计"}},
			{"title": "提交证据", "bullets": []string{"真实 PPTX 与自包含 HTML", "可预览、可下载、可复验"}},
		},
	}
	payloadBytes, err := json.Marshal(payload)
	require.NoError(t, err)
	args, err := json.Marshal(ExecuteSkillScriptInput{
		SkillName: "演示文稿生成器", ScriptPath: "scripts/generate_presentation.py", Input: string(payloadBytes),
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
			ArtifactType  string `json:"artifact_type"`
			PreviewFormat string `json:"preview_format"`
			MediaType     string `json:"media_type"`
		} `json:"artifacts"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &declared), stdout)
	require.True(t, declared.Success)
	require.Equal(t, []string{"presentation", "webpage"}, []string{declared.Artifacts[0].ArtifactType, declared.Artifacts[1].ArtifactType})
	require.Equal(t, []string{"pptx", "html"}, []string{declared.Artifacts[0].PreviewFormat, declared.Artifacts[1].PreviewFormat})
	require.NotEmpty(t, declared.Artifacts[0].MediaType)
	require.NotEmpty(t, declared.Artifacts[1].MediaType)

	store := manager.SessionFileStore()
	require.NotNil(t, store)
	outputDir := skillManager.SkillOutputDir("presentation-chain", "演示文稿生成器")
	entries, err := store.ListSessionFiles(ctx, "presentation-chain", outputDir)
	require.NoError(t, err)
	require.Len(t, entries, 3, "PPTX, HTML, and producer manifest must be emitted")

	require.NoError(t, os.MkdirAll(evidenceDir, 0o755))
	for _, name := range []string{"weknora-sandbox-demo.pptx", "weknora-sandbox-demo.html", ".weknora-artifacts.json"} {
		data, err := store.ReadSessionFile(ctx, "presentation-chain", outputDir+"/"+name)
		require.NoError(t, err)
		require.NotEmpty(t, data)
		require.NoError(t, os.WriteFile(filepath.Join(evidenceDir, name), data, 0o644))
		if strings.HasSuffix(name, ".pptx") {
			validatePresentationOOXML(t, data)
		}
		if strings.HasSuffix(name, ".html") {
			html := string(data)
			require.Contains(t, html, "<!doctype html>")
			require.Contains(t, html, "WeKnora 沙盒工作台实战")
			require.NotContains(t, html, "http://")
			require.NotContains(t, html, "https://")
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(evidenceDir, "tool-result.json"), []byte(fmt.Sprintf("%s\n", stdout)), 0o644))
}

func validatePresentationOOXML(t *testing.T, data []byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	names := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		names[f.Name] = true
	}
	require.True(t, names["[Content_Types].xml"])
	require.True(t, names["ppt/presentation.xml"])
	for i := 1; i <= 4; i++ {
		require.True(t, names[fmt.Sprintf("ppt/slides/slide%d.xml", i)])
	}
}
