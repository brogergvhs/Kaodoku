import base64
import html
import io
import json
import os
import posixpath
import re
import threading
import time
import zipfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

from selenium import webdriver
from selenium.webdriver.chrome.options import Options


IMAGE_MIME = ("image/jpeg", "image/png", "image/webp", "image/gif", "image/avif")
SKIP_TOKENS = ("/thumb/", "/banner/", "/avatar/", "/logo/", "/background", "fbshare", "share", "search")
CHAPTER_PATH = re.compile(r"(?:^|[/_-])(?:c|ch|chapter)[-_.]?\d", re.I)


def env(name, default):
    return os.environ.get(name, default)


def int_env(name, default):
    try:
        return int(env(name, str(default)))
    except ValueError:
        return default


def image_ext(url):
    path = urlparse(url).path.lower()
    ext = posixpath.splitext(path)[1].lstrip(".")
    return ext


def keep_image(url, mime, allowed):
    low = url.lower()
    if any(token in low for token in SKIP_TOKENS):
        return False
    ext = image_ext(url)
    if allowed and ext and ext not in allowed:
        return False
    if not mime:
        # DOM-sourced URLs carry no MIME; the checks above suffice.
        return True
    return mime.startswith(IMAGE_MIME) or (ext != "" and ext in allowed)


def safe_ext(url, mime):
    ext = image_ext(url)
    if ext:
        return ext
    if mime == "image/png":
        return "png"
    if mime == "image/webp":
        return "webp"
    if mime == "image/gif":
        return "gif"
    if mime == "image/avif":
        return "avif"
    return "jpg"


def driver_options():
    options = Options()
    options.add_argument("--headless=new")
    options.add_argument("--disable-gpu")
    options.add_argument("--no-sandbox")
    options.add_argument("--disable-dev-shm-usage")
    options.set_capability("goog:loggingPrefs", {"performance": "ALL"})
    return options


# One grid session per slot; unbounded sessions strand the grid on load spikes.
SESSIONS = threading.BoundedSemaphore(int_env("BROWSER_WORKER_MAX_SESSIONS", 1))


def open_driver(load_strategy=None):
    options = driver_options()
    if load_strategy:
        options.page_load_strategy = load_strategy
    driver = webdriver.Remote(command_executor=env("SELENIUM_REMOTE_URL", "http://selenium:4444/wd/hub"), options=options)
    timeout = int_env("BROWSER_WORKER_PAGE_LOAD_TIMEOUT_S", 120)
    driver.set_page_load_timeout(timeout)
    driver.set_script_timeout(timeout)
    return driver


def cdp(driver, cmd, params=None):
    params = params or {}
    if hasattr(driver, "execute_cdp_cmd"):
        return driver.execute_cdp_cmd(cmd, params)
    return driver.execute("executeCdpCommand", {"cmd": cmd, "params": params}).get("value", {})


def collect_new_images(driver, seen, allowed):
    images = []
    for entry in driver.get_log("performance"):
        try:
            msg = json.loads(entry["message"])["message"]
        except (KeyError, json.JSONDecodeError):
            continue
        if msg.get("method") != "Network.responseReceived":
            continue
        params = msg.get("params", {})
        response = params.get("response", {})
        url = response.get("url", "")
        mime = response.get("mimeType", "")
        request_id = params.get("requestId")
        if not request_id or url in seen or not keep_image(url, mime, allowed):
            continue
        try:
            body = cdp(driver, "Network.getResponseBody", {"requestId": request_id})
        except Exception:
            continue
        data = body.get("body", "")
        if body.get("base64Encoded"):
            data = base64.b64decode(data)
        else:
            data = data.encode()
        if not data:
            continue
        seen.add(url)
        images.append((url, mime, data))
    return images


def dom_image_urls(driver, allowed):
    nodes = driver.execute_script(
        """
        return Array.from(document.images).map((img, index) => {
            const rect = img.getBoundingClientRect();
            const attrs = ["currentSrc", "src", "data-src", "data-lazy-src", "data-original"];
            let url = "";
            for (const name of attrs) {
                url = name === "currentSrc" ? img.currentSrc : (name === "src" ? img.src : img.getAttribute(name));
                if (url) break;
            }
            return {
                url,
                index,
                top: rect.top + window.scrollY,
                left: rect.left + window.scrollX,
                width: img.naturalWidth || rect.width,
                height: img.naturalHeight || rect.height
            };
        });
        """
    )
    out = []
    seen = set()
    for node in sorted(nodes, key=lambda n: (n.get("top", 0), n.get("left", 0), n.get("index", 0))):
        url = node.get("url", "")
        if url in seen or not keep_image(url, "", allowed):
            continue
        seen.add(url)
        out.append(url)
    return out


def order_images(driver, images, allowed):
    by_url = {url: (url, mime, data) for url, mime, data in images}
    ordered = []
    for url in dom_image_urls(driver, allowed):
        if url in by_url:
            ordered.append(by_url[url])
    if ordered:
        return ordered
    return images


def scroll_and_capture(chapter_url, allowed):
    with SESSIONS:
        driver = open_driver()
        try:
            cdp(driver, "Network.enable")
            driver.get(chapter_url)
            check_blocked_page(driver)
            seen = set()
            images = []
            stable = 0
            last_count = 0
            max_scrolls = int_env("BROWSER_WORKER_MAX_SCROLLS", 80)
            idle_rounds = int_env("BROWSER_WORKER_IDLE_ROUNDS", 4)
            pause = int_env("BROWSER_WORKER_SCROLL_PAUSE_MS", 700) / 1000

            for _ in range(max_scrolls):
                time.sleep(pause)
                images.extend(collect_new_images(driver, seen, allowed))
                driver.execute_script("window.scrollBy(0, Math.max(window.innerHeight, 900));")
                if len(images) == last_count:
                    stable += 1
                else:
                    stable = 0
                    last_count = len(images)
                at_bottom = driver.execute_script("return window.innerHeight + window.scrollY >= document.body.scrollHeight - 4")
                if at_bottom and stable >= idle_rounds:
                    break
            else:
                print(f"warning: hit scroll ceiling ({max_scrolls}) before page bottom: {chapter_url}", flush=True)
            time.sleep(pause)
            images.extend(collect_new_images(driver, seen, allowed))
            return order_images(driver, images, allowed)
        finally:
            driver.quit()


def rendered_html(page_url):
    with SESSIONS:
        driver = open_driver()
        try:
            driver.get(page_url)
            check_blocked_page(driver)
            pause = int_env("BROWSER_WORKER_SCROLL_PAUSE_MS", 700) / 1000
            wait_for_render(driver, page_url, pause)
            if not looks_like_reader_url(page_url) and has_chapter_links(driver):
                return rendered_chapter_links(driver, pause)
            return rendered_images(driver, pause)
        finally:
            driver.quit()


def apply_session(driver, cookies, user_agent):
    if user_agent:
        cdp(driver, "Network.setUserAgentOverride", {"userAgent": user_agent})
    for c in cookies or []:
        params = {"name": c["name"], "value": c["value"], "domain": c.get("domain", ""), "path": c.get("path", "/")}
        if c.get("secure"):
            params["secure"] = True
        try:
            cdp(driver, "Network.setCookie", params)
        except Exception:
            pass


def intercept_requests(page_url, pattern, page_script, timeout_ms, max_requests, cookies=None, user_agent=None):
    rx = re.compile(pattern) if pattern else None
    limit = max_requests or 10
    with SESSIONS:
        # The payload arrives as a navigation while the page is still loading,
        # and that navigation can wedge the original load forever.
        driver = open_driver("none")
        try:
            cdp(driver, "Network.enable")
            apply_session(driver, cookies, user_agent)
            if page_script:
                cdp(driver, "Page.enable")
                cdp(driver, "Page.addScriptToEvaluateOnNewDocument", {"source": page_script})
            driver.get(page_url)
            deadline = time.time() + max(1, (timeout_ms or 30000) / 1000)
            # Results arrive as navigations whose payload rides in the URL
            # fragment; Network events strip fragments, Page events keep them.
            found = {}
            while time.time() < deadline and len(found) < limit:
                for entry in driver.get_log("performance"):
                    try:
                        msg = json.loads(entry["message"])["message"]
                    except (KeyError, ValueError):
                        continue
                    method = msg.get("method")
                    params = msg.get("params", {})
                    if method == "Network.requestWillBeSent":
                        req = params.get("request", {})
                        urls = [req.get("url", ""), params.get("documentURL", "")]
                    elif method in ("Page.frameRequestedNavigation", "Page.frameScheduledNavigation"):
                        req = {}
                        urls = [params.get("url", "")]
                    else:
                        continue
                    for url in urls:
                        if not url or (rx and not rx.search(url)):
                            continue
                        body = req.get("postData")
                        if body is None and req.get("hasPostData"):
                            try:
                                body = cdp(driver, "Network.getRequestPostData", {"requestId": params["requestId"]}).get("postData")
                            except Exception:
                                body = None
                        base = url.split("#")[0]
                        cur = found.get(base)
                        if cur is None:
                            found[base] = {"url": url, "method": req.get("method", "GET"), "headers": req.get("headers", {}), "body": body}
                        else:
                            if len(url) > len(cur["url"]):
                                cur["url"] = url
                            if body and not cur["body"]:
                                cur["body"] = body
                            if req.get("headers") and not cur["headers"]:
                                cur["headers"] = req["headers"]
                                cur["method"] = req.get("method", cur["method"])
                if not found:
                    try:
                        check_blocked_page(driver)
                    except RuntimeError:
                        raise
                    except Exception:
                        pass
                time.sleep(0.3)
            return list(found.values())[:limit]
        finally:
            driver.quit()


def evaluate_script(page_url, script, timeout_ms, cookies=None, user_agent=None):
    with SESSIONS:
        driver = open_driver()
        try:
            if timeout_ms:
                driver.set_script_timeout(max(1, timeout_ms / 1000))
            if page_url and page_url != "about:blank":
                cdp(driver, "Network.enable")
                apply_session(driver, cookies, user_agent)
                driver.get(page_url)
                check_blocked_page(driver)
            return driver.execute_script("return (" + script + ");")
        finally:
            driver.quit()


def check_blocked_page(driver):
    text = (driver.title + "\n" + driver.execute_script("return document.body ? document.body.innerText : ''")).lower()
    if "cloudflare" in text and ("sorry, you have been blocked" in text or "attention required" in text or "just a moment" in text):
        raise RuntimeError("browser fetch blocked by Cloudflare")


def looks_like_reader_url(page_url):
    return bool(CHAPTER_PATH.search(urlparse(page_url).path))


def has_chapter_links(driver):
    return driver.execute_script(
        """
        const roots = document.querySelectorAll(
            ".mpage__chapters,.mchap-list,.listing-chapters_wrap,.version-chap,.wp-manga-chapter,.chapter-list,.chapters,.eplister"
        );
        if (roots.length) return true;
        return Array.from(document.querySelectorAll("a[href]")).some(a =>
            /(chapter|\\bch\\.?\\s*\\d|\\/c\\d|chapter[-_]\\d)/i.test((a.href || "") + " " + (a.innerText || a.textContent || ""))
        );
        """
    )


def reader_image_count(driver):
    return driver.execute_script(
        """
        return Array.from(document.images).filter(img => {
            const url = img.currentSrc || img.src || img.getAttribute("data-src") ||
                img.getAttribute("data-lazy-src") || img.getAttribute("data-original") || "";
            return url && !/(\\/ads\\/|\\/covers\\/|\\/thumb\\/|logo|avatar|banner|fbshare|share|background)/i.test(url);
        }).length;
        """
    )


def wait_for_render(driver, page_url, pause):
    deadline = time.time() + int_env("BROWSER_WORKER_RENDER_WAIT_MS", 6000) / 1000
    while time.time() < deadline:
        ready = reader_image_count(driver) > 0 if looks_like_reader_url(page_url) else has_chapter_links(driver)
        if ready:
            return
        time.sleep(pause)


def rendered_chapter_links(driver, pause):
    links = []
    seen = set()
    max_pages = int_env("BROWSER_WORKER_MAX_HTML_PAGES", 40)

    for _ in range(max_pages):
        time.sleep(pause)
        for item in driver.execute_script(
            """
            const roots = Array.from(document.querySelectorAll(
                ".mpage__chapters,.mchap-list,.listing-chapters_wrap,.version-chap,.chapter-list,.chapters,.eplister"
            ));
            return (roots.length ? roots : [document]).flatMap(root => Array.from(root.querySelectorAll("a[href]")).map(a => ({
                href: a.href || a.getAttribute("href"),
                text: a.innerText || a.textContent || ""
            })));
            """
        ):
            key = (item.get("href", ""), item.get("text", "").strip())
            if key[0] and key not in seen:
                seen.add(key)
                links.append(key)
        clicked = driver.execute_script(
            """
            const next = Array.from(document.querySelectorAll("button[aria-label],a[aria-label]"))
                .find(el => /next page/i.test(el.getAttribute("aria-label") || "")
                    && !el.disabled
                    && el.getAttribute("aria-disabled") !== "true");
            if (!next) return false;
            next.click();
            return true;
            """
        )
        if not clicked:
            break
    else:
        print(f"warning: hit chapter page ceiling ({max_pages}); list may be truncated", flush=True)

    return "<html><body>" + "".join(
        f'<a href="{html.escape(href, quote=True)}">{html.escape(text)}</a>'
        for href, text in links
    ) + "</body></html>"


def rendered_images(driver, pause):
    images = []
    seen = set()
    max_scrolls = int_env("BROWSER_WORKER_MAX_SCROLLS", 80)
    idle_rounds = int_env("BROWSER_WORKER_IDLE_ROUNDS", 4)
    stable = 0
    last_count = -1

    for _ in range(max_scrolls):
        time.sleep(pause)
        for url in driver.execute_script(
            """
            return Array.from(document.images).map(img =>
                img.currentSrc || img.src || img.getAttribute("data-src") ||
                img.getAttribute("data-lazy-src") || img.getAttribute("data-original") || ""
            );
            """
        ):
            if url and url not in seen:
                seen.add(url)
                images.append(url)
        stable = stable + 1 if len(images) == last_count else 0
        last_count = len(images)
        at_bottom = driver.execute_script("return window.innerHeight + window.scrollY >= document.body.scrollHeight - 4")
        if at_bottom and stable >= idle_rounds:
            break
        driver.execute_script("window.scrollBy(0, Math.max(window.innerHeight, 900));")
    else:
        print(f"warning: hit scroll ceiling ({max_scrolls}); images may be truncated", flush=True)

    return "<html><body>" + "".join(
        f'<img src="{html.escape(url, quote=True)}" data-kaodoku-page-image="1">'
        for url in images
    ) + "</body></html>"


def make_cbz(images):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as zf:
        width = max(3, len(str(len(images))))
        for i, (url, mime, data) in enumerate(images, 1):
            zf.writestr(f"page_{i:0{width}d}.{safe_ext(url, mime)}", data)
    return buf.getvalue()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(fmt % args, flush=True)

    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
            return
        self.send_error(404)

    def do_POST(self):
        if self.path not in ("/download", "/html", "/evaluate", "/intercept"):
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            payload = json.loads(self.rfile.read(length))
            if self.path == "/intercept":
                started = time.time()
                page_url = payload["page_url"]
                print(f"intercept on {page_url} cookies={len(payload.get('cookies') or [])} ua={(payload.get('user_agent') or '')[:40]}", flush=True)
                requests = intercept_requests(
                    page_url, payload.get("url_pattern"), payload.get("page_script"),
                    int(payload.get("timeout_ms") or 0), int(payload.get("max_requests") or 0),
                    payload.get("cookies"), payload.get("user_agent"))
                print(f"intercept done {page_url} requests={len(requests)} elapsed={time.time() - started:.1f}s", flush=True)
                data = json.dumps({"requests": requests}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                return

            if self.path == "/evaluate":
                started = time.time()
                page_url = payload.get("page_url") or "about:blank"
                print(f"evaluate on {page_url}", flush=True)
                result = evaluate_script(page_url, payload["script"], int(payload.get("timeout_ms") or 0),
                                         payload.get("cookies"), payload.get("user_agent"))
                print(f"evaluate done {page_url} elapsed={time.time() - started:.1f}s", flush=True)
                data = json.dumps({"result": result}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                return

            if self.path == "/html":
                started = time.time()
                page_url = payload["page_url"]
                print(f"render html {page_url}", flush=True)
                data = rendered_html(page_url).encode()
                print(f"render html done {page_url} bytes={len(data)} elapsed={time.time() - started:.1f}s", flush=True)
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                return

            chapter_url = payload["chapter_url"]
            allowed = {ext.lower().lstrip(".") for ext in payload.get("allowed_extensions", [])}
            images = scroll_and_capture(chapter_url, allowed)
            if not images:
                raise RuntimeError("no browser-loaded images captured")
            cbz = make_cbz(images)
        except BrokenPipeError:
            return
        except Exception as exc:
            print(f"{self.path} failed: {exc}", flush=True)
            data = json.dumps({"error": str(exc)}).encode()
            self.send_response(502)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return

        self.send_response(200)
        self.send_header("Content-Type", "application/vnd.comicbook+zip")
        self.send_header("Content-Length", str(len(cbz)))
        self.send_header("X-Kaodoku-Images", str(len(images)))
        self.end_headers()
        self.wfile.write(cbz)


def main():
    host, port = env("BROWSER_WORKER_ADDR", "0.0.0.0:8192").rsplit(":", 1)
    ThreadingHTTPServer((host, int(port)), Handler).serve_forever()


if __name__ == "__main__":
    main()
