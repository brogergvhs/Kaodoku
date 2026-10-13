@file:OptIn(org.koitharu.kotatsu.parsers.InternalParsersApi::class)

package worker

import org.koitharu.kotatsu.parsers.model.MangaParserSource
import org.koitharu.kotatsu.parsers.model.MangaSource
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class ApiTest {
    private val api = Api(JvmLoaderContext(), "test-key")

    @Test
    fun signatureRoundTrip() {
        val sig = api.sign("MANGADEX", "https://x/img.jpg")
        assertTrue(api.verify(sig, "MANGADEX", "https://x/img.jpg"))
        assertFalse(api.verify(sig, "MANGADEX", "https://x/other.jpg"))
        assertFalse(api.verify(sig, "OTHER", "https://x/img.jpg"))
        assertFalse(api.verify(null, "MANGADEX", "https://x/img.jpg"))
        assertFalse(api.verify("", "MANGADEX", "https://x/img.jpg"))
    }

    @Test
    fun differentKeysDiffer() {
        val other = Api(JvmLoaderContext(), "other-key")
        assertFalse(other.verify(api.sign("S", "u"), "S", "u"))
    }

    @Test
    fun imagePathEncodesAndVerifies() {
        val path = api.imagePath("MANGADEX", "https://cdn.x/a b.jpg?v=1&k=2")
        assertTrue(path.startsWith("/image?source=MANGADEX&url=https%3A%2F%2Fcdn.x%2Fa+b.jpg%3Fv%3D1%26k%3D2&sig="))
        val sig = path.substringAfter("sig=")
        assertTrue(api.verify(sig, "MANGADEX", "https://cdn.x/a b.jpg?v=1&k=2"))
    }

    @Test
    fun parserDispatchAppliesParserHeaders() {
        val seen = java.util.concurrent.ConcurrentHashMap<String, String>()
        val server = com.sun.net.httpserver.HttpServer.create(java.net.InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/x") { ex ->
            ex.requestHeaders.forEach { (k, v) -> seen[k.lowercase()] = v.joinToString() }
            ex.sendResponseHeaders(204, -1)
            ex.close()
        }
        server.start()
        try {
            val ctx = JvmLoaderContext()
            val client = okhttp3.OkHttpClient.Builder().addInterceptor(ctx.parserDispatch).build()
            val url = "http://127.0.0.1:${server.address.port}/x"

            client.newCall(okhttp3.Request.Builder().url(url).build()).execute().close()
            val untagged = seen["user-agent"].orEmpty()
            assertTrue(untagged.startsWith("okhttp"), "expected okhttp default, got $untagged")

            seen.clear()
            client.newCall(
                okhttp3.Request.Builder().url(url)
                    .tag(MangaSource::class.java, MangaParserSource.CUUTRUYEN)
                    .build()
            ).execute().close()
            val tagged = seen["user-agent"].orEmpty()
            assertTrue(tagged.contains("Android 13"), "parser did not set its user agent: $seen")
            assertTrue(ctx.parserDispatch in ctx.httpClient.interceptors, "dispatch not installed on shared client")
        } finally {
            server.stop(0)
        }
    }

    @Test
    fun errorsMapToStatusAndCode() {
        val cases = listOf(
            CloudflareBlocked("https://x/y") to (503 to "cloudflare_challenge"),
            org.koitharu.kotatsu.parsers.exception.NotFoundException("nope", "https://x") to (404 to "not_found"),
            org.koitharu.kotatsu.parsers.exception.TooManyRequestExceptions("https://x", 0) to (429 to "rate_limited"),
            org.koitharu.kotatsu.parsers.exception.AuthRequiredException(MangaParserSource.MANGADEX) to (401 to "auth_required"),
            org.koitharu.kotatsu.parsers.exception.ContentUnavailableException("gone") to (410 to "content_unavailable"),
            org.koitharu.kotatsu.parsers.exception.ParseException("bad", "https://x", null) to (502 to "parse_failed"),
            org.jsoup.HttpStatusException("boom", 403, "https://x/y") to (502 to "upstream_http"),
            UnsupportedOperationException("no js") to (501 to "unsupported"),
            java.net.SocketTimeoutException("slow") to (504 to "upstream_timeout"),
            JsUnsupported() to (501 to "unsupported"),
            java.io.IOException("io") to (502 to "upstream_io"),
            IllegalStateException("boom") to (500 to "internal"),
        )
        for ((e, want) in cases) {
            val (status, body) = errorFor(e)
            assertEquals(want.first, status, "status for ${e::class.simpleName}")
            assertEquals(want.second, body.error, "code for ${e::class.simpleName}")
            assertTrue(body.message.isNotBlank())
        }
        val http = errorFor(org.jsoup.HttpStatusException("boom", 403, "https://x/y")).second
        assertEquals(403, http.upstream_status)
        assertEquals("https://x/y", http.url)
    }

    @Test
    fun handlerWritesErrorStatusAndJson() {
        val app = io.javalin.Javalin.create()
            .exception(Exception::class.java, ::handleError)
            .get("/boom") { throw CloudflareBlocked("https://x/y") }
            .start(0)
        try {
            val resp = okhttp3.OkHttpClient().newCall(
                okhttp3.Request.Builder().url("http://127.0.0.1:${app.port()}/boom?source=TEST").build()
            ).execute()
            val body = resp.use { it.body!!.string() }
            assertEquals(503, resp.code)
            assertTrue(body.contains("\"error\":\"cloudflare_challenge\""), body)
            assertTrue(body.contains("\"source\":\"TEST\""), body)
        } finally {
            app.stop()
        }
    }

    @Test
    fun cloudflareGuardThrowsOnChallenge() {
        val server = com.sun.net.httpserver.HttpServer.create(java.net.InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/x") { ex ->
            val body = """<html><body><div id="challenge-error-text">Just a moment</div></body></html>""".toByteArray()
            ex.sendResponseHeaders(403, body.size.toLong())
            ex.responseBody.use { it.write(body) }
        }
        server.createContext("/ok") { ex -> ex.sendResponseHeaders(204, -1); ex.close() }
        server.start()
        try {
            val ctx = JvmLoaderContext()
            val client = okhttp3.OkHttpClient.Builder().addInterceptor(ctx.cloudflareGuard).build()
            val base = "http://127.0.0.1:${server.address.port}"
            assertFailsWith<CloudflareBlocked> {
                client.newCall(okhttp3.Request.Builder().url("$base/x").build()).execute()
            }
            client.newCall(okhttp3.Request.Builder().url("$base/ok").build()).execute().use {
                assertEquals(204, it.code)
            }
            val installed = ctx.httpClient.interceptors
            assertTrue(ctx.cloudflareGuard in installed, "guard not installed")
            assertTrue(
                installed.indexOf(ctx.cloudflareGuard) < installed.indexOf(ctx.parserDispatch),
                "cloudflare guard must wrap parser dispatch",
            )
        } finally {
            server.stop(0)
        }
    }

    @Test
    fun privateAddressGuardBlocksLoopback() {
        val server = com.sun.net.httpserver.HttpServer.create(java.net.InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/x") { ex -> ex.sendResponseHeaders(200, 0); ex.close() }
        server.start()
        try {
            val call = JvmLoaderContext().httpClient.newCall(
                okhttp3.Request.Builder().url("http://127.0.0.1:${server.address.port}/x").build()
            )
            assertFailsWith<java.io.IOException> { call.execute() }
        } finally {
            server.stop(0)
        }
    }
}
