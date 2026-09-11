#!/usr/bin/env python3

from __future__ import annotations

import argparse
import base64
import io
import logging
import os
import re
import shutil
import sys
import tempfile
import urllib.error
import urllib.request
from pathlib import Path
from typing import NoReturn

ROOT = Path(__file__).resolve().parents[1]

# 全量字体只在 CI / 本地下载一次，页面里放的是子集；兜底 URL 供不支持 woff2 的浏览器使用。
FONT_URL = "https://raw.githubusercontent.com/lingyicute/Nebulove/main/Nebulove.ttf"
FALLBACK_URL = "https://cdn.jsdelivr.net/gh/lingyicute/Nebulove@main/Nebulove.ttf"
FONT_FAMILY = "Nebulove"

PAGE_DEFAULT = ROOT / "web" / "index.html"

# 全量字体的下载缓存（可用环境变量 NEBULOVE_TTF 覆盖），省掉每次 CI 的 2.9 MB 下载。
CACHE_TTF = Path(
    os.environ.get(
        "NEBULOVE_TTF", str(Path.home() / ".cache" / "nebulove" / "Nebulove.ttf")
    )
)

# 页面里可能没有、但排版上会用到的标点。
EXTRA_CHARS = "：，。「」『』（）【】—…·《》×＝＋－、！？；“”‘’"

# @font-face 块。CSS 里不会有嵌套花括号，[^{}]* 足够，并且能避开无关的 @font-face。
FACE_RE = re.compile(r"@font-face\s*\{[^{}]*\}", re.DOTALL)

# fontTools 会对它不认识的表（Nebulove 里的 FontForge 时间戳表 FFTM 等）打 warning，
# 这些都是子集化时正常丢弃的表，不必污染 CI 日志。
logging.getLogger("fontTools").setLevel(logging.ERROR)


def log(msg: str) -> None:
    print(msg, flush=True)


def die(msg: str, code: int = 1) -> NoReturn:
    print(f"Error: {msg}", file=sys.stderr)
    raise SystemExit(code)


def size_kb(text: str) -> float:
    """字符串按 UTF-8 编码后的 KB 数。"""
    return len(text.encode("utf-8")) / 1024


def collect_chars(html: str, extra: str) -> set[str]:
    """页面里出现过的每个字符 + 可打印 ASCII + 兜底标点，就是子集要覆盖的字符集。"""
    chars = set(html)
    chars.update(chr(c) for c in range(32, 127))
    chars.update(EXTRA_CHARS)
    chars.update(extra)
    return {c for c in chars if c not in "\r\n\t"}


def fetch_font(local: Path | None, url: str, refresh: bool) -> tuple[Path, bool]:
    """返回 (全量 TTF 路径, 是否为用完即删的临时文件)。"""
    if local is not None:
        if not local.is_file():
            die(f"font file not found: {local}")
        return local, False

    CACHE_TTF.parent.mkdir(parents=True, exist_ok=True)
    if CACHE_TTF.is_file() and not refresh:
        log(
            f"使用缓存字体 {CACHE_TTF}（{CACHE_TTF.stat().st_size / 1024 / 1024:.2f} MB）"
        )
        return CACHE_TTF, False

    log(f"下载字体 {url} ...")
    fd, tmp_name = tempfile.mkstemp(suffix=".ttf")
    os.close(fd)
    tmp = Path(tmp_name)
    try:
        with urllib.request.urlopen(url) as resp, tmp.open("wb") as f:
            shutil.copyfileobj(resp, f)
    except (urllib.error.URLError, OSError) as e:
        tmp.unlink(missing_ok=True)
        die(f"下载字体失败：{e}")
    if tmp.stat().st_size < 4096:
        tmp.unlink(missing_ok=True)
        die("下载到的字体文件过小，可能拿到了错误页面。")
    try:  # 放进缓存，下次（或 CI 下一次）就不必重新下载
        shutil.copyfile(tmp, CACHE_TTF)
        log(f"已缓存到 {CACHE_TTF}")
    except OSError:
        pass
    return tmp, True


def subset(ttf: Path, chars: set[str], out_dir: Path) -> tuple[Path, list[str]]:
    try:
        from fontTools.subset import Options, Subsetter
        from fontTools.ttLib import TTFont
    except ImportError:
        die("需要 fontTools：pip install -r scripts/requirements.txt")
    try:
        import brotli  # noqa: F401  woff2 压缩依赖
    except ImportError:
        die("需要 brotli（woff2 压缩）：pip install -r scripts/requirements.txt")

    # recalcTimestamp=False：否则 head.modified 会被刷成「当前时间」，同样的输入也会
    # 产出不同的字节，于是每跑一次页面就出现一次无意义的 diff。
    font = TTFont(ttf, lazy=False, recalcTimestamp=False)

    # 全量字体里就没有的字形，子集化不会凭空造出来——提前告知调用方。
    cmap = font.getBestCmap() or {}
    missing = sorted(c for c in chars if ord(c) not in cmap)

    log("子集化中 ...")
    subsetter = Subsetter(options=Options())
    subsetter.populate(text="".join(sorted(chars)))
    subsetter.subset(font)

    font.flavor = "woff2"
    out = out_dir / "Nebulove-Subset.woff2"
    font.save(out)
    return out, missing


def build_font_css(b64_font: str, fallback_url: str) -> str:
    return (
        "@font-face{\n"
        f'  font-family:"{FONT_FAMILY}";\n'
        f'  src:url("data:font/woff2;charset=utf-8;base64,{b64_font}") format("woff2"),\n'
        f'      url("{fallback_url}") format("truetype");\n'
        "  font-display:swap;\n"
        "}"
    )


def embed(html: str, font_css: str) -> str:
    """替换页面里属于 FONT_FAMILY 的 @font-face；一个都没有就插入新的 <style>。"""
    family_re = re.compile(re.escape(FONT_FAMILY), re.IGNORECASE)
    faces = [m for m in FACE_RE.finditer(html) if family_re.search(m.group(0))]

    if faces:
        first = faces[0]
        if len(faces) > 1:
            log(
                f"警告：页面里有 {len(faces)} 个引用 {FONT_FAMILY} 的 @font-face，只改写第一个。"
            )
        return html[: first.start()] + font_css + html[first.end() :]

    anchor = re.search(r"<style\b[^>]*>", html, re.IGNORECASE)
    if anchor is not None:
        pos = anchor.start()
        return html[:pos] + f"<style>\n{font_css}\n</style>\n" + html[pos:]

    anchor = re.search(r"</head>", html, re.IGNORECASE)
    if anchor is None:
        die("找不到插入 @font-face 的位置：页面既没有 <style> 也没有 </head>。")
    pos = anchor.start()
    return html[:pos] + f"<style>\n{font_css}\n</style>\n" + html[pos:]


DATA_URI_RE = re.compile(
    r"""url\(\s*['"]?data:font/woff2(?:;charset=[^;'"]*)?;base64,\s*([A-Za-z0-9+/=]+)\s*['"]?\)"""
)


def verify_embedded(html: str, wanted: set[str], not_in_full_font: set[str]) -> None:
    """把页面里内联的 data URI 反解成字体，确认它真的覆盖了页面用到的每个字符。"""
    try:
        from fontTools.ttLib import TTFont
    except ImportError:
        die("需要 fontTools：pip install -r scripts/requirements.txt")

    block = next(
        (
            m.group(0)
            for m in FACE_RE.finditer(html)
            if FONT_FAMILY.lower() in m.group(0).lower()
            and DATA_URI_RE.search(m.group(0))
        ),
        None,
    )
    if block is None:
        die("页面里找不到内联的 data:font/woff2，字体没有嵌进去。")

    font = TTFont(io.BytesIO(base64.b64decode(DATA_URI_RE.search(block).group(1))))
    cmap = font.getBestCmap() or {}
    fallback = wanted & not_in_full_font  # Nebulove 本来就没有的字符，交给回退字体
    uncovered = sorted(c for c in wanted - fallback if ord(c) not in cmap)
    if uncovered:
        die("内联的子集没有覆盖这些字符：" + "".join(uncovered))
    log(
        f"校验通过：内联子集覆盖 {len(wanted) - len(fallback)}/{len(wanted)} 个字符"
        + (
            f"（另外 {len(fallback)} 个 Nebulove 本身没有，走回退字体）"
            if fallback
            else ""
        )
    )


def parse_args(argv: list[str]) -> argparse.Namespace:

    p = argparse.ArgumentParser(description="Nebulove 字体子集化并内联进简介页")
    p.add_argument(
        "--page",
        type=Path,
        default=PAGE_DEFAULT,
        help="要改写的 HTML（默认 web/index.html）",
    )
    p.add_argument("--font", type=Path, default=None, help="本地全量 TTF，给了就不下载")
    p.add_argument(
        "--font-url", default=FONT_URL, help=f"全量字体下载地址（默认 {FONT_URL}）"
    )
    p.add_argument(
        "--fallback-url", default=FALLBACK_URL, help="写进 @font-face 的兜底字体 URL"
    )
    p.add_argument(
        "--extra", default="", help="额外强制纳入子集的字符（只在运行时出现的文案）"
    )
    p.add_argument("--refresh", action="store_true", help="忽略本地字体缓存，重新下载")
    p.add_argument(
        "--check",
        action="store_true",
        help="只检查页面里内联的子集是否为最新；过期则退出码 1，且不写文件",
    )
    p.add_argument(
        "--keep-woff2",
        type=Path,
        default=None,
        help="把子集 woff2 另存一份到该路径，便于单独托管",
    )
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    page: Path = args.page

    if not page.is_file():
        die(f"{page} not found.")
    html = page.read_text(encoding="utf-8")

    chars = collect_chars(html, args.extra)
    non_ascii = sum(1 for c in chars if ord(c) > 127)
    log(f"{page.name}：需要覆盖 {len(chars)} 个字符（非 ASCII {non_ascii} 个）")

    work = Path(tempfile.mkdtemp(prefix="nebulove-subset-"))
    missing: list[str] = []
    try:
        ttf, is_temp = fetch_font(args.font, args.font_url, args.refresh)
        full_size = ttf.stat().st_size
        try:
            woff2, missing = subset(ttf, chars, work)
        finally:
            if is_temp:
                ttf.unlink(missing_ok=True)

        size = woff2.stat().st_size
        log(
            f"子集 woff2：{size / 1024:.1f} KB（全量 TTF {full_size / 1024 / 1024:.2f} MB，压缩到 {size / full_size:.1%}）"
        )
        if missing:
            log(
                f"注意：Nebulove 没有这 {len(missing)} 个字符，它们会回退到后续字体：{''.join(missing)}"
            )

        if args.keep_woff2:
            args.keep_woff2.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(woff2, args.keep_woff2)
            log(f"子集另存到 {args.keep_woff2}")

        b64 = base64.b64encode(woff2.read_bytes()).decode("ascii")
        new_html = embed(html, build_font_css(b64, args.fallback_url))
    finally:
        shutil.rmtree(work, ignore_errors=True)

    # 回读一遍刚生成的内容：确认内联的字体的确覆盖了页面用到的字符。
    verify_embedded(new_html, chars, set(missing))

    if new_html == html:
        log(f"{page.name} 已是最新（内联子集 {len(b64) / 1024:.1f} KB），无需改动。")
        return 0

    if args.check:
        die(
            f"{page.name} 里的字体子集已过期，请运行 python3 scripts/subset_font.py 后重新提交。"
        )

    page.write_text(new_html, encoding="utf-8")
    log(
        f"已更新 {page.name}：内联子集 {len(b64) / 1024:.1f} KB，"
        f"页面体积 {size_kb(html):.1f} KB → {size_kb(new_html):.1f} KB"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
