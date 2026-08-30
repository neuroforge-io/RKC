#!/usr/bin/env python3
"""RKC Python AST extractor.

The extractor reads one JSON request from stdin and writes one GraphPatch-like
fragment to stdout. It intentionally uses only the Python standard library so
the reference implementation runs without installing a dependency festival.
"""

from __future__ import annotations

import ast
import hashlib
import json
import os
import stat
import sys
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Any, Iterable

PLUGIN_NAME = "rkc.python-ast"
PLUGIN_VERSION = "0.2.1"


def stable_id(namespace: str, *parts: str) -> str:
    key = namespace + "\0" + "\0".join(parts)
    digest = hashlib.sha256(key.encode("utf-8")).hexdigest()[:24]
    return f"rkc:{namespace}:{digest}"


def source_range(path: str, node: ast.AST, artifact_id: str = "") -> dict[str, Any]:
    return {
        "artifact_id": artifact_id,
        "path": path,
        "start_line": int(getattr(node, "lineno", 0) or 0),
        "start_column": int(getattr(node, "col_offset", 0) or 0),
        "end_line": int(getattr(node, "end_lineno", getattr(node, "lineno", 0)) or 0),
        "end_column": int(getattr(node, "end_col_offset", getattr(node, "col_offset", 0)) or 0),
    }


def unparse(node: ast.AST | None) -> str:
    if node is None:
        return ""
    try:
        return ast.unparse(node)
    except Exception:
        return node.__class__.__name__


def node_id(kind: str, path: str, qualified_name: str) -> str:
    return stable_id("node", kind, path, qualified_name)


def evidence_id(method: str, path: str, start: int, end: int, detail: str) -> str:
    return stable_id("evidence", method, path, str(start), str(end), detail)


def edge_id(kind: str, source: str, target: str) -> str:
    return stable_id("edge", kind, source, target)


def visibility(name: str) -> str:
    if name.startswith("__") and name.endswith("__"):
        return "special"
    if name.startswith("_"):
        return "private"
    return "public"


def module_name(path: str) -> str:
    value = path.replace("\\", "/")
    if value.endswith("/__init__.py"):
        value = value[: -len("/__init__.py")]
    elif value.endswith(".py"):
        value = value[:-3]
    return value.strip("/").replace("/", ".") or "__root__"


def signature_for_function(node: ast.FunctionDef | ast.AsyncFunctionDef) -> tuple[str, list[dict[str, Any]]]:
    args: list[dict[str, Any]] = []
    positional = [*node.args.posonlyargs, *node.args.args]
    default_offset = len(positional) - len(node.args.defaults)

    for index, arg in enumerate(positional):
        has_default = index >= default_offset
        default_node = node.args.defaults[index - default_offset] if has_default else None
        args.append(
            {
                "name": arg.arg,
                "kind": "positional_only" if index < len(node.args.posonlyargs) else "positional_or_keyword",
                "type": unparse(arg.annotation),
                "required": not has_default,
                "default": unparse(default_node) if has_default else "",
            }
        )
    if node.args.vararg:
        args.append(
            {
                "name": node.args.vararg.arg,
                "kind": "variadic_positional",
                "type": unparse(node.args.vararg.annotation),
                "required": False,
                "default": "",
            }
        )
    for arg, default_node in zip(node.args.kwonlyargs, node.args.kw_defaults):
        args.append(
            {
                "name": arg.arg,
                "kind": "keyword_only",
                "type": unparse(arg.annotation),
                "required": default_node is None,
                "default": unparse(default_node),
            }
        )
    if node.args.kwarg:
        args.append(
            {
                "name": node.args.kwarg.arg,
                "kind": "variadic_keyword",
                "type": unparse(node.args.kwarg.annotation),
                "required": False,
                "default": "",
            }
        )

    prefix = "async def" if isinstance(node, ast.AsyncFunctionDef) else "def"
    rendered_args: list[str] = []
    for arg in args:
        piece = arg["name"]
        if arg["kind"] == "variadic_positional":
            piece = "*" + piece
        elif arg["kind"] == "variadic_keyword":
            piece = "**" + piece
        if arg["type"]:
            piece += f": {arg['type']}"
        if arg["default"]:
            piece += f" = {arg['default']}"
        rendered_args.append(piece)
    returns = unparse(node.returns)
    signature = f"{prefix} {node.name}({', '.join(rendered_args)})"
    if returns:
        signature += f" -> {returns}"
    return signature, args


def callee_name(call: ast.Call) -> str:
    value = call.func
    if isinstance(value, ast.Name):
        return value.id
    if isinstance(value, ast.Attribute):
        parts: list[str] = []
        cursor: ast.AST = value
        while isinstance(cursor, ast.Attribute):
            parts.append(cursor.attr)
            cursor = cursor.value
        if isinstance(cursor, ast.Name):
            parts.append(cursor.id)
        return ".".join(reversed(parts))
    return unparse(value)


@dataclass
class Context:
    path: str
    artifact_id: str
    module_id: str
    module_name: str
    nodes: list[dict[str, Any]]
    edges: list[dict[str, Any]]
    evidence: list[dict[str, Any]]
    diagnostics: list[dict[str, Any]]

    def add_evidence(self, method: str, node: ast.AST, detail: str, kind: str = "declared", confidence: float = 1.0) -> str:
        source = source_range(self.path, node, self.artifact_id)
        item_id = evidence_id(method, self.path, source["start_line"], source["end_line"], detail)
        self.evidence.append(
            {
                "id": item_id,
                "kind": kind,
                "method": method,
                "confidence": confidence,
                "source": source,
                "tool": f"{PLUGIN_NAME}@{PLUGIN_VERSION}",
                "detail": detail,
            }
        )
        return item_id

    def add_edge(self, kind: str, source: str, target: str, resolution: str, evidence_ids: Iterable[str] = (), attributes: dict[str, Any] | None = None) -> None:
        self.edges.append(
            {
                "id": edge_id(kind, source, target),
                "kind": kind,
                "from": source,
                "to": target,
                "resolution": resolution,
                "confidence": 0.7 if resolution == "unresolved" else 1.0,
                "producer": PLUGIN_NAME,
                "evidence_ids": list(evidence_ids),
                "attributes": attributes or {},
            }
        )


def make_placeholder(ctx: Context, kind: str, name: str, namespace: str) -> str:
    placeholder_id = node_id(kind, ctx.path, f"{namespace}:{name}")
    if not any(item["id"] == placeholder_id for item in ctx.nodes):
        ctx.nodes.append(
            {
                "id": placeholder_id,
                "kind": kind,
                "name": name,
                "qualified_name": name,
                "language": "python",
                "visibility": "unknown",
                "attributes": {"placeholder": True, "namespace": namespace},
            }
        )
    return placeholder_id


class Extractor(ast.NodeVisitor):
    def __init__(self, ctx: Context) -> None:
        self.ctx = ctx
        self.parents: list[tuple[str, str, str]] = [(ctx.module_id, ctx.module_name, "module")]
        self.callers: list[str] = []

    @property
    def parent_id(self) -> str:
        return self.parents[-1][0]

    @property
    def parent_name(self) -> str:
        return self.parents[-1][1]

    @property
    def parent_kind(self) -> str:
        return self.parents[-1][2]

    def visit_ClassDef(self, node: ast.ClassDef) -> Any:
        qualified = f"{self.parent_name}.{node.name}"
        item_id = node_id("class", self.ctx.path, qualified)
        ev = self.ctx.add_evidence("python.ast.class", node, qualified)
        self.ctx.nodes.append(
            {
                "id": item_id,
                "logical_id": stable_id("logical", "python", "class", qualified),
                "kind": "class",
                "name": node.name,
                "qualified_name": qualified,
                "signature": f"class {node.name}({', '.join(unparse(base) for base in node.bases)})" if node.bases else f"class {node.name}",
                "language": "python",
                "visibility": visibility(node.name),
                "public_surface": visibility(node.name) == "public",
                "artifact_id": self.ctx.artifact_id,
                "source": source_range(self.ctx.path, node, self.ctx.artifact_id),
                "evidence_ids": [ev],
                "attributes": {
                    "docstring": ast.get_docstring(node) or "",
                    "decorators": [unparse(item) for item in node.decorator_list],
                    "bases": [unparse(item) for item in node.bases],
                },
            }
        )
        self.ctx.add_edge("contains", self.parent_id, item_id, "declared", [ev])
        for base in node.bases:
            base_name = unparse(base)
            target = make_placeholder(self.ctx, "unresolved_symbol", base_name, "base")
            self.ctx.add_edge("inherits", item_id, target, "unresolved", [ev], {"spelling": base_name})
        self.parents.append((item_id, qualified, "class"))
        self.generic_visit(node)
        self.parents.pop()
        return None

    def _visit_function(self, node: ast.FunctionDef | ast.AsyncFunctionDef) -> Any:
        in_class = self.parent_kind == "class"
        kind = "method" if in_class else ("test" if node.name.startswith("test_") else "function")
        qualified = f"{self.parent_name}.{node.name}"
        item_id = node_id(kind, self.ctx.path, qualified)
        signature, arguments = signature_for_function(node)
        ev = self.ctx.add_evidence("python.ast.function", node, qualified)
        self.ctx.nodes.append(
            {
                "id": item_id,
                "logical_id": stable_id("logical", "python", kind, qualified),
                "kind": kind,
                "name": node.name,
                "qualified_name": qualified,
                "signature": signature,
                "language": "python",
                "visibility": visibility(node.name),
                "public_surface": visibility(node.name) == "public",
                "artifact_id": self.ctx.artifact_id,
                "source": source_range(self.ctx.path, node, self.ctx.artifact_id),
                "evidence_ids": [ev],
                "attributes": {
                    "arguments": arguments,
                    "returns": unparse(node.returns),
                    "docstring": ast.get_docstring(node) or "",
                    "decorators": [unparse(item) for item in node.decorator_list],
                    "async": isinstance(node, ast.AsyncFunctionDef),
                },
            }
        )
        self.ctx.add_edge("contains", self.parent_id, item_id, "declared", [ev])
        self.parents.append((item_id, qualified, kind))
        self.callers.append(item_id)
        self.generic_visit(node)
        self.callers.pop()
        self.parents.pop()
        return None

    def visit_FunctionDef(self, node: ast.FunctionDef) -> Any:
        return self._visit_function(node)

    def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> Any:
        return self._visit_function(node)

    def visit_Import(self, node: ast.Import) -> Any:
        ev = self.ctx.add_evidence("python.ast.import", node, unparse(node))
        for alias in node.names:
            target = make_placeholder(self.ctx, "external_dependency", alias.name, "import")
            self.ctx.add_edge("imports", self.ctx.module_id, target, "declared", [ev], {"alias": alias.asname or ""})
        return None

    def visit_ImportFrom(self, node: ast.ImportFrom) -> Any:
        ev = self.ctx.add_evidence("python.ast.import", node, unparse(node))
        module = "." * node.level + (node.module or "")
        target = make_placeholder(self.ctx, "external_dependency", module, "import")
        self.ctx.add_edge(
            "imports",
            self.ctx.module_id,
            target,
            "declared",
            [ev],
            {"names": [alias.name for alias in node.names], "aliases": [alias.asname or "" for alias in node.names]},
        )
        return None

    def visit_Call(self, node: ast.Call) -> Any:
        if self.callers:
            spelling = callee_name(node)
            target = make_placeholder(self.ctx, "unresolved_symbol", spelling, "call")
            ev = self.ctx.add_evidence("python.ast.call", node, spelling, kind="syntax_inferred", confidence=0.7)
            self.ctx.add_edge("calls", self.callers[-1], target, "unresolved", [ev], {"spelling": spelling})
        self.generic_visit(node)
        return None


def canonical_file_path(value: Any) -> str:
    if not isinstance(value, str) or not value or "\\" in value or "\0" in value:
        raise ValueError("plugin file path must be a canonical slash-separated relative path")
    pure = PurePosixPath(value)
    if pure.is_absolute() or pure.as_posix() != value or value == ".":
        raise ValueError("plugin file path must be a canonical slash-separated relative path")
    if any(part in {"", ".", ".."} for part in value.split("/")):
        raise ValueError("plugin file path must be a canonical slash-separated relative path")
    return value


def canonical_file_digest(value: Any) -> str:
    if not isinstance(value, str) or len(value) != 64 or value != value.lower():
        raise ValueError("plugin file SHA-256 must be 64 lowercase hexadecimal characters")
    try:
        decoded = bytes.fromhex(value)
    except ValueError as exc:
        raise ValueError("plugin file SHA-256 must be 64 lowercase hexadecimal characters") from exc
    if len(decoded) != hashlib.sha256().digest_size:
        raise ValueError("plugin file SHA-256 must be 64 lowercase hexadecimal characters")
    return value


def read_verified_source(root: Path, file_ref: dict[str, Any]) -> bytes:
    if not isinstance(file_ref, dict):
        raise ValueError("plugin file reference must be an object")
    path = canonical_file_path(file_ref.get("path"))
    advertised_size = file_ref.get("size_bytes")
    if advertised_size is not None and (
        isinstance(advertised_size, bool)
        or not isinstance(advertised_size, int)
        or advertised_size < 0
        or advertised_size > sys.maxsize
    ):
        raise ValueError(f"plugin file {path!r} has an invalid size")
    expected_digest = canonical_file_digest(file_ref.get("sha256"))

    candidate = root
    path_info = None
    parts = path.split("/")
    for index, part in enumerate(parts):
        candidate = candidate / part
        path_info = candidate.lstat()
        if stat.S_ISLNK(path_info.st_mode):
            raise ValueError(f"plugin file {path!r} contains a symlink")
        if index < len(parts) - 1 and not stat.S_ISDIR(path_info.st_mode):
            raise ValueError(f"plugin file {path!r} contains a non-directory component")
    if path_info is None or not stat.S_ISREG(path_info.st_mode):
        raise ValueError(f"plugin file {path!r} is not a regular file")
    size = path_info.st_size if advertised_size is None else advertised_size
    if advertised_size is not None and path_info.st_size != size:
        raise ValueError(f"plugin file {path!r} size does not match inventory")
    try:
        candidate.resolve(strict=True).relative_to(root)
    except (OSError, ValueError) as exc:
        raise ValueError(f"plugin file {path!r} escapes the request root") from exc

    content = bytearray()
    digest = hashlib.sha256()
    with candidate.open("rb") as source:
        opened_info = os.fstat(source.fileno())
        if not stat.S_ISREG(opened_info.st_mode) or not os.path.samestat(path_info, opened_info):
            raise ValueError(f"plugin file {path!r} identity changed while opening")
        remaining = size
        while remaining:
            chunk = source.read(min(64 * 1024, remaining))
            if not chunk:
                raise ValueError(f"plugin file {path!r} became shorter than inventory")
            content.extend(chunk)
            digest.update(chunk)
            remaining -= len(chunk)
        if source.read(1):
            raise ValueError(f"plugin file {path!r} grew beyond inventory")
        final_info = os.fstat(source.fileno())
    if (
        not stat.S_ISREG(final_info.st_mode)
        or not os.path.samestat(opened_info, final_info)
        or final_info.st_size != opened_info.st_size
        or final_info.st_mtime_ns != opened_info.st_mtime_ns
    ):
        raise ValueError(f"plugin file {path!r} identity changed while reading")
    final_path_info = candidate.lstat()
    if (
        stat.S_ISLNK(final_path_info.st_mode)
        or not stat.S_ISREG(final_path_info.st_mode)
        or not os.path.samestat(opened_info, final_path_info)
    ):
        raise ValueError(f"plugin file {path!r} path identity changed while reading")
    if digest.hexdigest() != expected_digest:
        raise ValueError(f"plugin file {path!r} content does not match inventory")
    return bytes(content)


def process_file(file_ref: dict[str, Any], source_bytes: bytes) -> dict[str, list[dict[str, Any]]]:
    path = canonical_file_path(file_ref["path"])
    artifact_id = str(file_ref["id"])
    nodes: list[dict[str, Any]] = []
    edges: list[dict[str, Any]] = []
    evidence: list[dict[str, Any]] = []
    diagnostics: list[dict[str, Any]] = []
    module_qualified = module_name(path)
    module_id = node_id("module", path, module_qualified)

    try:
        source = source_bytes.decode("utf-8")
        tree = ast.parse(source, filename=path, type_comments=True)
    except (UnicodeError, SyntaxError) as exc:
        line = int(getattr(exc, "lineno", 0) or 0)
        diagnostics.append(
            {
                "id": stable_id("diagnostic", "python_parse", path, str(line), str(exc)),
                "severity": "error",
                "code": "RKC-PY-1001",
                "message": str(exc),
                "source": {"artifact_id": artifact_id, "path": path, "start_line": line},
                "stage": "syntax_parse",
                "plugin": f"{PLUGIN_NAME}@{PLUGIN_VERSION}",
            }
        )
        return {"nodes": nodes, "edges": edges, "evidence": evidence, "diagnostics": diagnostics}

    module_ev = evidence_id("python.ast.module", path, 1, max(1, len(source.splitlines())), module_qualified)
    evidence.append(
        {
            "id": module_ev,
            "kind": "declared",
            "method": "python.ast.module",
            "confidence": 1.0,
            "source": {"artifact_id": artifact_id, "path": path, "start_line": 1, "end_line": max(1, len(source.splitlines()))},
            "tool": f"{PLUGIN_NAME}@{PLUGIN_VERSION}",
            "detail": module_qualified,
        }
    )
    nodes.append(
        {
            "id": module_id,
            "logical_id": stable_id("logical", "python", "module", module_qualified),
            "kind": "module",
            "name": module_qualified.rsplit(".", 1)[-1],
            "qualified_name": module_qualified,
            "language": "python",
            "visibility": visibility(module_qualified.rsplit(".", 1)[-1]),
            "public_surface": visibility(module_qualified.rsplit(".", 1)[-1]) == "public",
            "artifact_id": artifact_id,
            "source": {"artifact_id": artifact_id, "path": path, "start_line": 1, "end_line": max(1, len(source.splitlines()))},
            "evidence_ids": [module_ev],
            "attributes": {"docstring": ast.get_docstring(tree) or ""},
        }
    )
    edges.append(
        {
            "id": edge_id("contains", artifact_id, module_id),
            "kind": "contains",
            "from": artifact_id,
            "to": module_id,
            "resolution": "declared",
            "confidence": 1.0,
            "producer": PLUGIN_NAME,
            "evidence_ids": [module_ev],
            "attributes": {},
        }
    )

    ctx = Context(path, artifact_id, module_id, module_qualified, nodes, edges, evidence, diagnostics)
    Extractor(ctx).visit(tree)
    return {"nodes": nodes, "edges": edges, "evidence": evidence, "diagnostics": diagnostics}


def main() -> int:
    request = json.load(sys.stdin)
    root_value = request.get("root")
    if not isinstance(root_value, str) or not root_value:
        raise ValueError("plugin request root must be a nonempty path")
    root_path = Path(root_value)
    root_path_info = root_path.lstat()
    if stat.S_ISLNK(root_path_info.st_mode) or not stat.S_ISDIR(root_path_info.st_mode):
        raise ValueError("plugin request root must be a real directory, not a symlink")
    root = root_path.resolve(strict=True)
    opened_root_info = root.stat()
    if not stat.S_ISDIR(opened_root_info.st_mode) or not os.path.samestat(root_path_info, opened_root_info):
        raise ValueError("plugin request root identity changed while opening")
    file_refs = request.get("files", [])
    if not isinstance(file_refs, list):
        raise ValueError("plugin request files must be a list")
    for file_ref in file_refs:
        if not isinstance(file_ref, dict):
            raise ValueError("plugin file reference must be an object")
        canonical_file_path(file_ref.get("path"))
    result: dict[str, list[dict[str, Any]]] = {"nodes": [], "edges": [], "evidence": [], "diagnostics": []}
    for file_ref in sorted(file_refs, key=lambda item: item["path"]):
        source = read_verified_source(root, file_ref)
        if file_ref.get("language") != "python":
            continue
        fragment = process_file(file_ref, source)
        for key in result:
            result[key].extend(fragment[key])
    final_root_info = root_path.lstat()
    if (
        stat.S_ISLNK(final_root_info.st_mode)
        or not stat.S_ISDIR(final_root_info.st_mode)
        or not os.path.samestat(opened_root_info, final_root_info)
    ):
        raise ValueError("plugin request root identity changed while reading")
    for key in result:
        result[key].sort(key=lambda item: item.get("id", ""))
    json.dump(result, sys.stdout, sort_keys=True, separators=(",", ":"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
