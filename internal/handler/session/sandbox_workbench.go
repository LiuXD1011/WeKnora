package session

import (
	stderrors "errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/gin-gonic/gin"
)

type sandboxRenameRequest struct {
	OldPath string `json:"old_path" binding:"required"`
	NewPath string `json:"new_path" binding:"required"`
}

func workbenchSessionID(c *gin.Context) string {
	if id := strings.TrimSpace(c.Param("id")); id != "" {
		return id
	}
	return strings.TrimSpace(c.Param("session_id"))
}

func (h *Handler) requireWorkbench(c *gin.Context) *service.SandboxWorkbenchService {
	if h == nil || h.sandboxWorkbench == nil {
		c.Error(apperrors.NewServiceUnavailableError("sandbox workbench is not configured"))
		return nil
	}
	return h.sandboxWorkbench
}

func writeWorkbenchError(c *gin.Context, err error) {
	switch {
	case stderrors.Is(err, service.ErrSandboxWorkbenchPath):
		c.Error(apperrors.NewBadRequestError(err.Error()))
	case stderrors.Is(err, service.ErrSandboxWorkbenchNotReady):
		c.Error(apperrors.NewConflictError(err.Error()))
	case stderrors.Is(err, service.ErrSandboxWorkbenchUnsupported):
		c.Error(apperrors.NewServiceUnavailableError(err.Error()))
	default:
		logger.ErrorWithFields(c.Request.Context(), err, map[string]interface{}{
			"session_id": workbenchSessionID(c),
		})
		c.Error(apperrors.NewInternalServerError(err.Error()))
	}
}

// GetSandboxWorkbenchInfo reports the session-pinned provider and effective
// capabilities without exposing a provider sandbox ID.
func (h *Handler) GetSandboxWorkbenchInfo(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	info, err := workbench.Info(c.Request.Context(), workbenchSessionID(c))
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": info})
}

// ListSandboxWorkbenchFiles lists files below /workspace/output. The client
// supplies a relative directory only; absolute provider paths are never
// accepted or returned.
func (h *Handler) ListSandboxWorkbenchFiles(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	files, err := workbench.ListFiles(
		c.Request.Context(), workbenchSessionID(c), c.Query("path"),
	)
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": files})
}

// DownloadSandboxWorkbenchFile streams one live artifact after the owned
// session and output-root checks have succeeded.
func (h *Handler) DownloadSandboxWorkbenchFile(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	relativePath := c.Query("path")
	data, _, err := workbench.ReadFile(
		c.Request.Context(), workbenchSessionID(c), relativePath,
	)
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	name := filepath.Base(filepath.FromSlash(relativePath))
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("X-Content-Type-Options", "nosniff")
	if c.Query("disposition") == "inline" {
		c.Header("Content-Disposition", "inline; filename="+fmt.Sprintf("%q", name))
		// If an HTML file is ever opened as a top-level response, keep it unable
		// to reach WeKnora APIs. The UI additionally renders it in a sandboxed
		// iframe with no allow-same-origin token.
		if strings.EqualFold(filepath.Ext(name), ".html") || strings.EqualFold(filepath.Ext(name), ".htm") {
			c.Header("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; img-src data: blob:; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		}
	} else {
		c.Header("Content-Disposition", buildAttachmentHeader(name))
	}
	c.Data(http.StatusOK, contentType, data)
}

// UploadSandboxWorkbenchFile writes one multipart upload below the artifact
// root. Existing files are replaced deliberately, matching ordinary file
// manager semantics.
func (h *Handler) UploadSandboxWorkbenchFile(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.Error(apperrors.NewBadRequestError("file is required"))
		return
	}
	defer file.Close()
	if header.Size > service.SandboxWorkbenchMaxUploadBytes {
		c.Error(apperrors.NewBadRequestError("file exceeds sandbox workbench upload limit"))
		return
	}
	relativePath := strings.TrimSpace(c.PostForm("path"))
	if relativePath == "" {
		relativePath = filepath.ToSlash(filepath.Base(header.Filename))
	}
	limited := io.LimitReader(file, service.SandboxWorkbenchMaxUploadBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	if int64(len(data)) > service.SandboxWorkbenchMaxUploadBytes {
		c.Error(apperrors.NewBadRequestError("file exceeds sandbox workbench upload limit"))
		return
	}
	if err := workbench.WriteFile(c.Request.Context(), workbenchSessionID(c), relativePath, data); err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": gin.H{"path": relativePath}})
}

func (h *Handler) RenameSandboxWorkbenchFile(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	var request sandboxRenameRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	if err := workbench.RenameFile(
		c.Request.Context(), workbenchSessionID(c), request.OldPath, request.NewPath,
	); err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) DeleteSandboxWorkbenchFile(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	if err := workbench.DeleteFile(
		c.Request.Context(), workbenchSessionID(c), c.Query("path"),
	); err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ExecuteSandboxWorkbenchCommand executes inside the server-selected session
// sandbox. Request cancellation interrupts the provider call through ctx.
func (h *Handler) ExecuteSandboxWorkbenchCommand(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	var request service.SandboxWorkbenchCommand
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	result, err := workbench.ExecuteCommand(
		c.Request.Context(), workbenchSessionID(c), request,
	)
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}
