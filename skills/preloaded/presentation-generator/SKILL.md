---
name: 演示文稿生成器
description: 将结构化提纲生成可下载的 PPTX 和可在沙箱工作台内联预览的 HTML 演示文稿。当用户要求制作汇报、路演、课件或幻灯片时使用。
---

# Presentation Generator

将标题、副标题和多页要点生成为两个一致的产物：

- `presentation.pptx`：可下载编辑的 PowerPoint 文件；
- `presentation.html`：无外部依赖的内联预览，用于沙箱工作台的隔离 iframe。

## 调用方式

调用 `execute_skill_script`：

```json
{
  "skill_name": "演示文稿生成器",
  "script_path": "scripts/generate_presentation.py",
  "input": "{\"title\":\"项目汇报\",\"subtitle\":\"可视化沙箱工作台\",\"slides\":[{\"title\":\"背景\",\"bullets\":[\"用户无法看到沙箱执行过程\",\"产物只能下载\"]}]}"
}
```

## 输入格式

```json
{
  "title": "必填，演示文稿标题",
  "subtitle": "可选，副标题",
  "author": "可选，作者",
  "output_name": "可选，默认 presentation",
  "slides": [
    {
      "title": "页面标题",
      "bullets": ["要点一", "要点二"]
    }
  ]
}
```

## 产物约定

1. 脚本必须将文件写到 `WEKNORA_SKILL_OUTPUT_DIR`。
2. 脚本返回 JSON，其中列出两个产物的相对路径、类型和用途。
3. HTML 产物不请求网络资源，不读取 Cookie 或主站存储。
4. 工作台将 HTML 放入不带 `allow-same-origin` 的 sandbox iframe。

## 生成步骤

1. 根据用户目标拟定 5–12 页结构化提纲。
2. 每页只保留一个主题，建议不超过 6 条要点。
3. 将完整 JSON 通过 `input` 传给脚本。
4. 检查返回的 `artifacts` 列表。
5. 在答案中同时告知用户 PPTX 可下载、HTML 可预览。

## 约束

- 页面数量限制为 1–30 页。
- 单页要点最多 10 条，超出部分会截断。
- 输出名称只使用字母、数字、下划线和连字号。
