#!/usr/bin/env python3
"""Generate a styled XLSX artifact without third-party Python packages."""

from __future__ import annotations

import json
import math
import os
import re
import sys
import zipfile
from datetime import datetime, timezone
from pathlib import Path
from xml.sax.saxutils import escape, quoteattr


OUTPUT_ENV = "WEKNORA_SKILL_OUTPUT_DIR"
MANIFEST_NAME = ".weknora-artifacts.json"
NAME_RE = re.compile(r"[^A-Za-z0-9_-]+")


def read_payload() -> dict:
    raw = sys.stdin.read()
    if not raw.strip():
        raise ValueError("input JSON is required")
    value = json.loads(raw)
    if not isinstance(value, dict):
        raise ValueError("input must be a JSON object")
    return value


def normalise(payload: dict) -> tuple[str, str, str, list[str], list[list[object]]]:
    title = str(payload.get("title", "")).strip() or "数据表"
    sheet_name = re.sub(r"[\\/*?:\[\]]", "_", str(payload.get("sheet_name", "Sheet1")).strip())[:31] or "Sheet1"
    output_name = NAME_RE.sub("-", str(payload.get("output_name", "spreadsheet"))).strip("-") or "spreadsheet"
    columns = payload.get("columns")
    rows = payload.get("rows")
    if not isinstance(columns, list) or not columns:
        raise ValueError("columns must be a non-empty array")
    if len(columns) > 200:
        raise ValueError("columns exceeds 200")
    clean_columns = [str(value) for value in columns]
    if not isinstance(rows, list):
        raise ValueError("rows must be an array")
    if len(rows) > 10000:
        raise ValueError("rows exceeds 10000")
    clean_rows: list[list[object]] = []
    for row in rows:
        if not isinstance(row, list):
            raise ValueError("each row must be an array")
        clean_rows.append(list(row[: len(clean_columns)]) + [""] * max(0, len(clean_columns) - len(row)))
    return output_name, title, sheet_name, clean_columns, clean_rows


def column_name(index: int) -> str:
    result = ""
    while index:
        index, remainder = divmod(index - 1, 26)
        result = chr(65 + remainder) + result
    return result


def cell_xml(ref: str, value: object, style: int = 0) -> str:
    style_attr = f' s="{style}"' if style else ""
    if value is None:
        return f'<c r="{ref}"{style_attr}/>'
    if isinstance(value, bool):
        return f'<c r="{ref}" t="b"{style_attr}><v>{1 if value else 0}</v></c>'
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        if isinstance(value, float) and not math.isfinite(value):
            value = str(value)
        else:
            return f'<c r="{ref}"{style_attr}><v>{value}</v></c>'
    text = str(value)
    preserve = ' xml:space="preserve"' if text[:1].isspace() or text[-1:].isspace() else ""
    return f'<c r="{ref}" t="inlineStr"{style_attr}><is><t{preserve}>{escape(text)}</t></is></c>'


def worksheet_xml(title: str, columns: list[str], rows: list[list[object]]) -> str:
    width_rows = [columns] + [[str(value) for value in row] for row in rows[:500]]
    widths = []
    for idx in range(len(columns)):
        longest = max((len(str(row[idx])) for row in width_rows if idx < len(row)), default=8)
        widths.append(min(50, max(10, longest + 2)))
    last_col = column_name(len(columns))
    last_row = len(rows) + 2
    sheet_rows = [f'<row r="1" ht="28" customHeight="1">{cell_xml("A1", title, 1)}</row>']
    sheet_rows.append('<row r="2">' + ''.join(cell_xml(f"{column_name(i + 1)}2", value, 2) for i, value in enumerate(columns)) + '</row>')
    for row_index, row in enumerate(rows, start=3):
        cells = ''.join(cell_xml(f"{column_name(i + 1)}{row_index}", value) for i, value in enumerate(row))
        sheet_rows.append(f'<row r="{row_index}">{cells}</row>')
    cols = ''.join(f'<col min="{i}" max="{i}" width="{width}" customWidth="1"/>' for i, width in enumerate(widths, start=1))
    return (
        '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>'
        '<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">'
        f'<dimension ref="A1:{last_col}{last_row}"/>'
        '<sheetViews><sheetView workbookViewId="0"><pane ySplit="2" topLeftCell="A3" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>'
        f'<cols>{cols}</cols><sheetData>{"".join(sheet_rows)}</sheetData>'
        f'<mergeCells count="1"><mergeCell ref="A1:{last_col}1"/></mergeCells>'
        f'<autoFilter ref="A2:{last_col}{last_row}"/>'
        '</worksheet>'
    )


def write_xlsx(output: Path, title: str, sheet_name: str, columns: list[str], rows: list[list[object]]) -> None:
    now = datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    entries = {
        "[Content_Types].xml": '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/><Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/><Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/></Types>',
        "_rels/.rels": '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/></Relationships>',
        "xl/workbook.xml": f'<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><bookViews><workbookView/></bookViews><sheets><sheet name={quoteattr(sheet_name)} sheetId="1" r:id="rId1"/></sheets><calcPr calcId="191029" fullCalcOnLoad="1"/></workbook>',
        "xl/_rels/workbook.xml.rels": '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>',
        "xl/worksheets/sheet1.xml": worksheet_xml(title, columns, rows),
        "xl/styles.xml": '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="3"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="16"/><color rgb="FF0F2440"/><name val="Microsoft YaHei"/></font><font><b/><sz val="11"/><color rgb="FFFFFFFF"/><name val="Microsoft YaHei"/></font></fonts><fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FF07C05F"/><bgColor indexed="64"/></patternFill></fill></fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/><xf numFmtId="0" fontId="2" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>',
        "docProps/core.xml": f'<?xml version="1.0" encoding="UTF-8" standalone="yes"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><dc:title>{escape(title)}</dc:title><dc:creator>WeKnora</dc:creator><dcterms:created xsi:type="dcterms:W3CDTF">{now}</dcterms:created></cp:coreProperties>',
        "docProps/app.xml": '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"><Application>WeKnora</Application></Properties>',
    }
    temporary = output.with_suffix(output.suffix + ".tmp")
    with zipfile.ZipFile(temporary, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for name, content in entries.items():
            archive.writestr(name, content)
    temporary.replace(output)


def main() -> int:
    try:
        output_name, title, sheet_name, columns, rows = normalise(read_payload())
        output_dir = Path(os.environ.get(OUTPUT_ENV, "/workspace/output"))
        output_dir.mkdir(parents=True, exist_ok=True)
        xlsx_path = output_dir / f"{output_name}.xlsx"
        write_xlsx(xlsx_path, title, sheet_name, columns, rows)
        artifacts = [{
            "path": xlsx_path.name,
            "type": "spreadsheet",
            "artifact_type": "spreadsheet",
            "preview_format": "spreadsheet",
            "media_type": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
            "purpose": "preview-download",
        }]
        manifest = output_dir / MANIFEST_NAME
        temporary_manifest = output_dir / f"{MANIFEST_NAME}.tmp"
        temporary_manifest.write_text(json.dumps({"version": 1, "artifacts": artifacts}, ensure_ascii=False, indent=2), encoding="utf-8")
        temporary_manifest.replace(manifest)
        print(json.dumps({"success": True, "artifacts": artifacts}, ensure_ascii=False))
        return 0
    except Exception as exc:  # noqa: BLE001
        print(json.dumps({"success": False, "error": str(exc)}, ensure_ascii=False), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
