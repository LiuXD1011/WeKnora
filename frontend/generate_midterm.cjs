const {
  Document, Packer, Paragraph, TextRun, Table, TableRow, TableCell,
  Footer, PageNumber, AlignmentType, HeadingLevel, WidthType,
  BorderStyle, ShadingType, ImageRun,
} = require("docx");
const fs = require("fs");

const BLACK = "000000";
const GRAY_FILL = "F2F2F2";
const BORDER = { style: BorderStyle.SINGLE, size: 4, color: "808080" };
const TBL_BORDERS = {
  top: BORDER, bottom: BORDER, left: BORDER, right: BORDER,
  insideHorizontal: BORDER, insideVertical: BORDER,
};
const FONT_BODY = { ascii: "Times New Roman", eastAsia: "SimSun" };
const FONT_HEAD = { ascii: "Times New Roman", eastAsia: "SimHei" };
const SHOT = "C:/WORK/\u5f00\u6e90\u5b9e\u4e60/WeKnora/frontend/e2e-artifacts/";

function h1(text) {
  return new Paragraph({
    heading: HeadingLevel.HEADING_1,
    keepNext: true,
    spacing: { before: 320, after: 140, line: 340, lineRule: "atLeast" },
    children: [new TextRun({ text, bold: true, size: 28, font: FONT_HEAD, color: BLACK })],
  });
}
function h2(text) {
  return new Paragraph({
    heading: HeadingLevel.HEADING_2,
    keepNext: true,
    spacing: { before: 220, after: 90, line: 312 },
    children: [new TextRun({ text, bold: true, size: 24, font: FONT_HEAD, color: BLACK })],
  });
}
function body(text, opts) {
  const o = opts || {};
  return new Paragraph({
    alignment: AlignmentType.JUSTIFIED,
    indent: { firstLine: 480 },
    spacing: { line: 312, after: o.after !== undefined ? o.after : 20 },
    keepNext: o.keepNext || false,
    children: [new TextRun({ text, size: 24, font: FONT_BODY, color: BLACK })],
  });
}
function bodyRuns(runs, opts) {
  const o = opts || {};
  return new Paragraph({
    alignment: AlignmentType.JUSTIFIED,
    indent: { firstLine: 480 },
    spacing: { line: 312, after: 20 },
    children: runs,
  });
}
function cellPara(text, bold) {
  return new Paragraph({
    alignment: AlignmentType.LEFT,
    spacing: { line: 300 },
    children: [new TextRun({ text, bold: !!bold, size: 21, font: FONT_BODY, color: BLACK })],
  });
}
function cell(text, widthPct, opts) {
  const o = opts || {};
  return new TableCell({
    children: [cellPara(text, o.bold)],
    margins: { top: 50, bottom: 50, left: 110, right: 110 },
    width: { size: widthPct, type: WidthType.PERCENTAGE },
    shading: o.fill ? { type: ShadingType.CLEAR, fill: o.fill } : undefined,
  });
}
function headerRow(labels, widths) {
  return new TableRow({
    tableHeader: true,
    cantSplit: true,
    children: labels.map((t, i) => cell(t, widths[i], { bold: true, fill: GRAY_FILL })),
  });
}
function dataRow(values, widths) {
  return new TableRow({
    cantSplit: true,
    children: values.map((t, i) => cell(t, widths[i])),
  });
}
function pngSize(path) {
  const buf = fs.readFileSync(path);
  return { w: buf.readUInt32BE(16), h: buf.readUInt32BE(20) };
}

function shot(name, caption, widthPt) {
  // Read the real pixel size so the aspect ratio is always preserved.
  const { w, h } = pngSize(SHOT + name);
  const width = widthPt || 300;
  const height = Math.round((width * h) / w);
  return [
    new Paragraph({
      alignment: AlignmentType.CENTER,
      keepNext: true,
      spacing: { before: 80, after: 40 },
      children: [new ImageRun({
        type: "png",
        data: fs.readFileSync(SHOT + name),
        transformation: { width, height },
      })],
    }),
    new Paragraph({
      alignment: AlignmentType.CENTER,
      spacing: { after: 120, line: 280 },
      children: [new TextRun({ text: caption, size: 21, font: FONT_KAI(), color: "595959" })],
    }),
  ];
}
function FONT_KAI() {
  return { ascii: "Times New Roman", eastAsia: "KaiTi" };
}

const tW = [18, 30, 52];
const testW = [30, 46, 24];

const children = [
  new Paragraph({
    alignment: AlignmentType.CENTER,
    spacing: { before: 60, after: 60, line: 414, lineRule: "atLeast" },
    children: [new TextRun({ text: "\u8bfe\u9898\u4e8c \u00b7 \u53ef\u89c6\u5316\u6c99\u7bb1\u5de5\u4f5c\u53f0 \u2014\u2014 \u4e2d\u671f\u6c47\u62a5", bold: true, size: 36, font: FONT_HEAD, color: BLACK })],
  }),
  new Paragraph({
    alignment: AlignmentType.CENTER,
    spacing: { after: 200, line: 312 },
    children: [new TextRun({ text: "\u817e\u8baf\u7280\u725b\u9e1f\u5f00\u6e90\u4eba\u624d\u57f9\u517b\u8ba1\u5212 \u00b7 WeKnora \u5f00\u6e90\u5b9e\u6218\u8bfe\u9898\uff082026 \u5e74 9 \u6708\uff09", size: 21, font: FONT_KAI(), color: BLACK })],
  }),

  h1("\u4e00\u3001\u9009\u9898\u4e0e\u6210\u679c\u5f62\u6001"),
  body("\u9009\u9898\uff1a\u8bfe\u9898\u4e8c\u2014\u2014\u53ef\u89c6\u5316\u6c99\u7bb1\u5de5\u4f5c\u53f0\uff08\u4ea7\u54c1\u529f\u80fd\u65b9\u5411\uff0c\u9ad8\u96be\u5ea6\uff09\u3002\u76ee\u6807\u662f\u628a\u4f1a\u8bdd\u5df2\u7ed1\u5b9a\u7684 Docker / CubeSandbox / E2B \u6c99\u7bb1\u6620\u5c04\u4e3a\u7edf\u4e00\u7684\u7ec8\u7aef\u3001\u6587\u4ef6\u4e0e\u4ea7\u54c1\u9884\u89c8\u754c\u9762\uff0c\u8ba9 Agent \u7684\u80fd\u529b\u8fb9\u754c\u4ece\u9ed1\u76d2\u53d8\u4e3a\u53ef\u4ea4\u4e92\u3001\u53ef\u9884\u89c8\u3001\u53ef\u5ba1\u8ba1\u3002"),
  body("\u4e00\u53e5\u8bdd\u65b9\u6848\uff1a\u4e0d\u53e6\u8d77\u4e00\u5957\u6c99\u7bb1\u3002\u5728\u4e3b\u5206\u652f\u73b0\u6709\u7684\u4f1a\u8bdd\u7ed1\u5b9a\u6c99\u7bb1\u4e0e\u80fd\u529b\u63a5\u53e3\u4e4b\u4e0a\u589e\u52a0\u53ef\u9009\u7684\u4ea4\u4e92\u7ec8\u7aef\u80fd\u529b\uff0c\u590d\u7528\u65e2\u6709\u7684\u79df\u6237\u9694\u79bb\u3001\u8def\u5f84\u7b56\u7565\u4e0e\u5ba1\u8ba1\u57fa\u7840\u8bbe\u65bd\uff0c\u628a\u7ec8\u7aef\u3001\u6587\u4ef6\u3001\u4ea7\u54c1\u9884\u89c8\u7edf\u4e00\u6210\u540c\u4e00\u4f1a\u8bdd\u7684\u53f3\u4fa7\u5de5\u4f5c\u53f0\uff1b\u6d4f\u89c8\u5668\u59cb\u7ec8\u53ea\u63d0\u4f9b\u4f1a\u8bdd ID\uff0c\u4e0d\u63a5\u89e6\u5e95\u5c42\u6c99\u7bb1 ID\u3002"),
  body("\u6210\u679c\u5f62\u6001\uff1a\u53ef\u5408\u5165\u4e3b\u4ed3\u7684\u5206\u652f PR\uff08\u5df2\u63a8\u9001\u81f3\u4e2a\u4eba fork\uff09+ \u8bbe\u8ba1\u6587\u6863 + Playwright E2E \u4e0e\u540e\u7aef\u6d4b\u8bd5\u8bc1\u636e + \u6f14\u793a\u811a\u672c\u3002"),

  h1("\u4e8c\u3001\u5df2\u5b8c\u6210\u5de5\u4f5c"),
  body("\u5206\u652f feat/visual-sandbox-workbench \u5171 9 \u4e2a\u63d0\u4ea4\uff0c\u6309\u67b6\u6784\u5206\u5c42\u63d0\u4ea4\u5e76\u5df2\u63a8\u9001\u3002\u529f\u80fd\u5168\u90e8\u5b9e\u73b0\uff1a"),
  body("1. \u4ea4\u4e92\u5f0f PTY \u7ec8\u7aef\uff08Docker\uff09\uff1axterm.js + WebSocket \u53cc\u5411\u6d41\uff0c\u5b9e\u65f6\u8f93\u51fa\u3001\u7a97\u53e3\u81ea\u9002\u5e94 resize\u3001Ctrl-C \u4e2d\u65ad\uff1b30 \u5206\u949f\u786c\u79df\u7ea6\u5230\u671f\u81ea\u52a8\u7ec8\u6b62\u5e76\u63d0\u793a\u539f\u56e0\uff1b\u6bcf\u4f1a\u8bdd\u6700\u591a 2 \u4e2a\u7ec8\u7aef\uff1b\u7a7a\u95f2\u6807\u8bb0\u4fdd\u6d3b\u9632\u6b62\u5bb9\u5668\u88ab\u56de\u6536\u3002"),
  body("2. \u80fd\u529b\u534f\u5546\u964d\u7ea7\uff1a\u540e\u7aef\u901a\u8fc7\u80fd\u529b\u63a5\u53e3\u4e0a\u62a5\u662f\u5426\u652f\u6301 PTY\uff1b\u4e0d\u652f\u6301\u7684\u540e\u7aef\uff08Cube/E2B\uff09\u81ea\u52a8\u964d\u7ea7\u4e3a\u547d\u4ee4\u6a21\u5f0f\u5e76\u660e\u793a\u63d0\u793a\uff0c\u4e0d\u4f2a\u88c5\u6210\u4ea4\u4e92\u7ec8\u7aef\uff1b\u964d\u7ea7\u8def\u5f84\u540c\u6837\u6ee1\u8db3\u9a8c\u6536\u7684\u300c\u4e24\u79cd\u540e\u7aef\u7ec8\u7aef\u53ef\u7528\u300d\u3002"),
  body("3. \u4ea7\u7269\u6587\u4ef6\u7ba1\u7406\uff1a\u76ee\u5f55\u5bfc\u822a\uff08\u9762\u5305\u5c51/\u5b50\u76ee\u5f55/\u8fd4\u56de\u4e0a\u7ea7\uff09\u3001\u4e0a\u4f20\u3001\u4e0b\u8f7d\u3001\u91cd\u547d\u540d\u3001\u5220\u9664\uff1b\u8def\u5f84\u53cc\u9632\u7ebf\uff08\u8bcd\u6cd5\u6e05\u6d17 + \u6c99\u7bb1\u5185 realpath \u590d\u6838\uff09\uff0c\u8d8a\u754c\u3001\u7b26\u53f7\u94fe\u63a5\u3001\u7a7a\u5b57\u8282\u3001\u7f16\u7801\u7ed5\u8fc7\u5168\u90e8\u62d2\u7edd\u5e76\u5ba1\u8ba1\u3002"),
  body("4. \u4e09\u7c7b\u4ea7\u7269\u5185\u8054\u9884\u89c8\uff1a\u6f14\u793a\u6587\u7a3f\uff08vue-office\uff09\u4e0e\u81ea\u5305\u542b HTML \u9010\u9875\u653e\u6620\uff1b\u7f51\u9875\u8fd0\u884c\u5728\u65e0 allow-same-origin \u7684\u53d7\u9650 iframe + \u670d\u52a1\u7aef CSP\uff1b\u8868\u683c\u89e3\u6790\u4e3a\u53ea\u8bfb\u8868\u683c\u89c6\u56fe\uff1b\u53e6\u652f\u6301 PDF/\u56fe\u7247/\u6587\u672c\u3002"),
  body("5. \u6f14\u793a\u6587\u7a3f Skill\uff08presentation-generator\uff09\uff1apython-pptx \u751f\u6210\u53ef\u7f16\u8f91 PPTX + \u81ea\u5305\u542b HTML \u9884\u89c8\uff0c\u6253\u901a\u300cAgent \u751f\u6210 \u2192 \u5de5\u4f5c\u53f0\u51fa\u73b0 \u2192 \u5728\u7ebf\u9884\u89c8 \u2192 \u4e0b\u8f7d\u300d\u9ec4\u91d1\u94fe\u8def\u3002"),
  body("6. \u6cbb\u7406\u4e0e\u5b89\u5168\uff1a\u6240\u6709\u64cd\u4f5c\u5148\u8fc7 GetOwnedSession\uff08\u7528\u6237+\u79df\u6237\u53cc\u95e8\uff09\uff1bWebSocket \u51ed\u8bc1\u4ee5 bearer \u5b50\u534f\u8bae\u6865\u63a5\uff08token \u4e0d\u8fdb URL\uff09\uff1b\u547d\u4ee4\u884c\u3001\u7ec8\u7aef\u5f00\u5173\u3001\u6587\u4ef6\u53d8\u66f4\u5168\u91cf\u5199\u5165\u79df\u6237\u5ba1\u8ba1\u65e5\u5fd7\u3002"),
  body("7. \u6587\u6863\uff1adocs/sandbox-workbench.md \u5b8c\u6574\u8bb0\u5f55\u67b6\u6784\u3001WS \u534f\u8bae\u3001\u80fd\u529b\u534f\u5546\u3001\u5b89\u5168\u8fb9\u754c\u4e0e\u9a8c\u8bc1\u547d\u4ee4\u3002"),

  h1("\u4e09\u3001\u53ef\u5c55\u793a\u6210\u679c\uff08E2E \u5b9e\u6d4b\u622a\u56fe\uff09"),
  body("\u4ee5\u4e0b\u622a\u56fe\u7531 Playwright \u9a71\u52a8\u771f\u5b9e\u524d\u7aef\u7ec4\u4ef6\uff08\u542b xterm.js \u7ec8\u7aef\u4e0e\u5b8c\u6574\u63a5\u53e3\u94fe\u8def\uff09\u81ea\u52a8\u5316\u622a\u53d6\uff0c\u975e\u624b\u5de5\u62fc\u56fe\u3002"),
  ...shot("terminal-interactive.png", "\u56fe 1  \u4ea4\u4e92\u5f0f\u7ec8\u7aef\uff1axterm.js \u5b9e\u65f6\u6d41\uff0c\u72b6\u6001\u680f\u663e\u793a\u540e\u7aef\u7c7b\u578b\u4e0e\u7ec8\u7aef ID"),
  ...shot("files-navigation.png", "\u56fe 2  \u4ea7\u7269\u6587\u4ef6\u7ba1\u7406\uff1a\u76ee\u5f55\u5bfc\u822a\u3001\u4e0a\u4f20/\u4e0b\u8f7d/\u91cd\u547d\u540d/\u5220\u9664"),
  ...shot("preview-html.png", "\u56fe 3  \u7f51\u9875\u4ea7\u7269\u5728\u65e0 allow-same-origin \u7684\u53d7\u9650 iframe \u5185\u9884\u89c8"),
  ...shot("terminal-degraded.png", "\u56fe 4  Cube \u540e\u7aef\u81ea\u52a8\u964d\u7ea7\u4e3a\u547d\u4ee4\u6a21\u5f0f\uff0c\u5e76\u660e\u793a\u964d\u7ea7\u539f\u56e0"),

  h1("\u56db\u3001\u6d4b\u8bd5\u8bc1\u636e"),
  body("\u540e\u7aef\uff08Go\uff0cWSL/Linux \u73af\u5883\u6267\u884c\uff09\u4e0e\u524d\u7aef\u5747\u5df2\u5168\u91cf\u9a8c\u8bc1\uff1a", { keepNext: true, after: 60 }),
  new Table({
    width: { size: 100, type: WidthType.PERCENTAGE },
    borders: TBL_BORDERS,
    rows: [
      headerRow(["\u5957\u4ef6", "\u8986\u76d6\u5185\u5bb9", "\u7ed3\u679c"], testW),
      dataRow(["internal/sandbox \u00b7 PTY \u5951\u7ea6", "TTY exec\u3001stdin/stdout \u6d41\u3001resize\u3001\u975e\u8fd0\u884c\u5bb9\u5668\u6062\u590d\u3001Wait \u4e0a\u4e0b\u6587", "4/4 \u901a\u8fc7"], testW),
      dataRow(["\u5de5\u4f5c\u53f0\u670d\u52a1", "\u8def\u5f84\u8d8a\u754c/\u7b26\u53f7\u94fe\u63a5/\u7a7a\u5b57\u8282/\u7f16\u7801\u7ed5\u8fc7\u3001\u53cc\u540e\u7aef\u5217\u8868\u3001\u79df\u6237\u9694\u79bb\u3001\u7ec8\u7aef\u4e0a\u9650\u4e0e\u5ba1\u8ba1", "13/13 \u901a\u8fc7"], testW),
      dataRow(["WS handler \u5168\u94fe\u8def", "ready \u2192 \u952e\u5165\u56de\u663e \u2192 resize \u8f6c\u53d1 \u2192 \u65ad\u5f00\u91ca\u653e\u69fd\u4f4d\uff08httptest + gorilla \u5ba2\u6237\u7aef\uff09", "3/3 \u901a\u8fc7"], testW),
      dataRow(["\u8ba4\u8bc1\u6865\u63a5", "bearer \u5b50\u534f\u8bae\u63d0\u5347\u4e3a Authorization\uff1btenant_id \u67e5\u8be2\u53c2\u6570\u63d0\u5347\u4e3a X-Tenant-ID", "\u901a\u8fc7"], testW),
      dataRow(["\u56de\u5f52", "handler/session \u5168\u91cf + router \u5168\u91cf + go build/vet \u5168\u4ed3", "\u901a\u8fc7"], testW),
      dataRow(["\u524d\u7aef", "vue-tsc \u7c7b\u578b\u68c0\u67e5\u3001vite \u6784\u5efa\u3001\u5355\u5143\u6d4b\u8bd5", "\u901a\u8fc7"], testW),
      dataRow(["Playwright E2E", "\u771f\u5b9e\u7ec4\u4ef6\u9a71\u52a8\uff1a\u4ea4\u4e92\u7ec8\u7aef\u3001\u964d\u7ea7\u3001\u76ee\u5f55\u5bfc\u822a\u3001\u53d7\u9650 iframe\u3001CSV \u89c6\u56fe\u3001\u7a7a\u6001", "7/7 \u901a\u8fc7"], testW),
    ],
  }),
  body("\u8bf4\u660e\uff1a\u672c\u673a Windows \u56e0 gojieba\uff08cgo\uff09\u521d\u59cb\u5316\u5d29\u6e83\u65e0\u6cd5\u8fd0\u884c Go \u6d4b\u8bd5\uff0c\u5df2\u5728 WSL Ubuntu 20.04 + go1.27 \u4e0b\u9a8c\u8bc1\uff1bservice \u5305\u4e2d MCP/QueryTemplates \u4e0e sandbox \u5305\u7684 DNS \u63a2\u6d4b\u5931\u8d25\u5728\u5e72\u51c0 main \u5206\u652f\u4e0a\u540c\u6837\u5b58\u5728\uff0c\u4e3a\u73af\u5883\u4f9d\u8d56\uff0c\u4e0e\u672c\u8bfe\u9898\u6539\u52a8\u65e0\u5173\u3002", { after: 40 }),

  h1("\u4e94\u3001\u5f53\u524d\u5361\u70b9"),
  body("1. Cube/E2B \u7684 envd \u4f20\u8f93\u5c42\u5f53\u524d\u4e0d\u63d0\u4f9b\u6d41\u5f0f PTY \u901a\u9053\uff0c\u4ea4\u4e92\u7ec8\u7aef\u6682\u4ec5 Docker \u652f\u6301\uff08\u80fd\u529b\u534f\u5546\u4e0e\u964d\u7ea7\u5df2\u5b8c\u6210\uff0c\u63a5\u5165\u70b9\u5df2\u9884\u7559\uff09\u3002"),
  body("2. \u672c\u5730\u65e0 Docker daemon \u65f6\uff0c\u771f\u5b9e\u5bb9\u5668\u94fe\u8def\u7684\u6f14\u793a\u4f9d\u8d56\u4e00\u53f0\u53ef\u8fd0\u884c Docker \u7684\u73af\u5883\uff1bE2E \u5c42\u9762\u5df2\u7528\u6d4f\u89c8\u5668\u62e6\u622a\u65b9\u5f0f\u8986\u76d6\u5168\u90e8\u524d\u7aef\u884c\u4e3a\u3002"),
  body("3. LibreOffice headless\uff08PPTX \u8f6c\u56fe\u9884\u89c8\uff09\u4f1a\u589e\u5927\u6c99\u7bb1\u955c\u50cf\u4f53\u79ef\uff0c\u5f53\u524d\u4ee5 Skill \u4ea7\u51fa\u81ea\u5305\u542b HTML \u9884\u89c8\u4f5c\u4e3a\u66ff\u4ee3\u3002"),

  h1("\u516d\u3001\u9700\u8981\u5bfc\u5e08\u786e\u8ba4\u7684\u95ee\u9898"),
  body("1. \u7b2c\u4e8c\u540e\u7aef\u4f18\u5148\u63a5\u5165 Cube \u8fd8\u662f E2B\uff1f\uff08\u5f71\u54cd 9/9 \u7684\u7b2c\u4e8c\u540e\u7aef\u9a8c\u6536\u8def\u7ebf\uff09"),
  body("2. \u4ea4\u4e92\u6a21\u5f0f\u4e0b\u5ba1\u8ba1\u8bb0\u5f55\u7528\u6237\u952e\u5165\u7684\u547d\u4ee4\u884c\uff08best effort\uff09\u662f\u5426\u6ee1\u8db3\u300c\u6bcf\u6761\u547d\u4ee4\u53ef\u67e5\u8be2\u300d\u7684\u9a8c\u6536\u8981\u6c42\uff0c\u8fd8\u662f\u9700\u8981 shell \u7ea7\u5386\u53f2\u843d\u76d8\u3002"),
  body("3. PPTX \u9010\u9875\u56fe\u7247\u9884\u89c8\u662f\u5426\u5141\u8bb8\u5c06 LibreOffice headless \u56fa\u5b9a\u8fdb\u5b98\u65b9\u6c99\u7bb1\u955c\u50cf\uff08\u955c\u50cf\u4f53\u79ef\u4e0e\u51b7\u542f\u52a8\u4f1a\u589e\u52a0\uff09\u3002"),

  h1("\u4e03\u3001\u540e\u7eed\u8ba1\u5212\uff08\u81f3 9 \u6708 13 \u65e5\u622a\u6b62\uff09", ),
  body("9/5\uff1a\u53cc\u79df\u6237\u5e76\u53d1\u9694\u79bb\u96c6\u6210\u6d4b\u8bd5\uff1b9/6\uff1a\u4ea7\u7269 manifest \u4e0e\u663e\u5f0f\u53d1\u5e03\u52a8\u4f5c\uff1b9/7\uff1a\u7f51\u9875\u9884\u89c8\u5b89\u5168\u5934\u6076\u610f fixture \u6d4b\u8bd5\uff1b9/8\uff1a\u6f14\u793a\u6587\u7a3f Skill \u94fe\u8def\u6253\u78e8\u4e0e\u6f14\u793a\u5f55\u5c4f\uff1b9/9\uff1a\u7b2c\u4e8c\u540e\u7aef\u63a5\u5165\uff08\u6309\u5bfc\u5e08\u610f\u89c1\uff09\uff1b9/10\uff1a\u8d44\u6e90\u9650\u5236\u4e0e\u7ec8\u6b62\u8def\u5f84\u9a8c\u8bc1\uff1b9/11\uff1a\u5ba1\u8ba1\u56de\u5f52\u4e0e\u6587\u6863\u6536\u5c3e\uff1b9/12\uff1a\u6253\u6700\u7ec8 Tag\u3001\u751f\u6210 submission.yaml\u3001\u53d1\u9001\u63d0\u4ea4\u90ae\u4ef6\u3002", { after: 0 }),
];

const doc = new Document({
  styles: {
    default: {
      document: {
        run: { font: FONT_BODY, size: 24, color: BLACK },
        paragraph: { spacing: { line: 312 } },
      },
      heading1: { run: { font: FONT_HEAD, size: 28, bold: true, color: BLACK } },
      heading2: { run: { font: FONT_HEAD, size: 24, bold: true, color: BLACK } },
    },
  },
  sections: [{
    properties: {
      page: {
        size: { width: 11906, height: 16838 },
        margin: { top: 1440, bottom: 1440, left: 1701, right: 1417 },
      },
    },
    footers: {
      default: new Footer({
        children: [new Paragraph({
          alignment: AlignmentType.CENTER,
          spacing: { line: 240 },
          children: [
            new TextRun({ text: "\u2014 ", size: 18, font: FONT_BODY, color: BLACK }),
            new TextRun({ children: [PageNumber.CURRENT], size: 18, font: FONT_BODY, color: BLACK }),
            new TextRun({ text: " \u2014", size: 18, font: FONT_BODY, color: BLACK }),
          ],
        })],
      }),
    },
    children,
  }],
});

Packer.toBuffer(doc).then(buf => {
  fs.writeFileSync("C:/WORK/\u5f00\u6e90\u5b9e\u4e60/\u63d0\u4ea4\u6750\u6599/\u8bfe\u9898\u4e8c\u4e2d\u671f\u6c47\u62a5.docx", buf);
  console.log("OK bytes:", buf.length);
});
