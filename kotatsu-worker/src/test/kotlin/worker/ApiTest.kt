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
            io.javalin.http.NotFoundResponse("unknown source") to (404 to "not_found"),
            io.javalin.http.BadRequestResponse("url required") to (400 to "bad_request"),
            io.javalin.http.UnauthorizedResponse() to (401 to "unauthorized"),
            io.javalin.http.BadGatewayResponse("upstream 500") to (502 to "upstream_http"),
            io.javalin.http.HttpResponseException(418, "teapot") to (418 to "http_418"),
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
            .exception(io.javalin.http.HttpResponseException::class.java, ::handleError)
            .exception(Exception::class.java, ::handleError)
            .get("/boom") { throw CloudflareBlocked("https://x/y") }
            .get("/missing") { throw io.javalin.http.NotFoundResponse("manga not found on source") }
            .start(0)
        try {
            val client = okhttp3.OkHttpClient()
            val resp = client.newCall(
                okhttp3.Request.Builder().url("http://127.0.0.1:${app.port()}/boom?source=TEST").build()
            ).execute()
            val body = resp.use { it.body!!.string() }
            assertEquals(503, resp.code)
            assertTrue(body.contains("\"error\":\"cloudflare_challenge\""), body)
            assertTrue(body.contains("\"source\":\"TEST\""), body)

            val nf = client.newCall(
                okhttp3.Request.Builder().url("http://127.0.0.1:${app.port()}/missing?source=TEST").build()
            ).execute()
            val nfBody = nf.use { it.body!!.string() }
            assertEquals(404, nf.code)
            assertTrue(nfBody.contains("\"error\":\"not_found\""), nfBody)
            assertTrue(nfBody.contains("manga not found on source"), nfBody)
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
    fun solverClearsChallengeAndPinsAgent() {
        val solves = java.util.concurrent.atomic.AtomicInteger()
        val cleared = java.util.concurrent.atomic.AtomicBoolean()
        val agents = java.util.Collections.synchronizedList(mutableListOf<String>())
        val site = challengeServer(cleared, agents)
        val solver = solverServer(solves, site.address.port)
        try {
            val ctx = JvmLoaderContext(FlareSolverr("http://127.0.0.1:${solver.address.port}", 5_000))
            val client = okhttp3.OkHttpClient.Builder()
                .cookieJar(ctx.cookieJar)
                .addInterceptor(ctx.cloudflareGuard)
                .addInterceptor(ctx.solvedAgentPin)
                .build()
            val url = "http://127.0.0.1:${site.address.port}/x"

            client.newCall(okhttp3.Request.Builder().url(url).build()).execute().use {
                assertEquals(204, it.code)
            }
            assertEquals(1, solves.get())
            assertEquals("solved-agent", agents.last())

            client.newCall(okhttp3.Request.Builder().url(url).build()).execute().use {
                assertEquals(204, it.code)
            }
            assertEquals(1, solves.get(), "clearance should be reused")
        } finally {
            site.stop(0)
            solver.stop(0)
        }
    }

    @Test
    fun unsolvableHostStopsRetryingSolves() {
        val solves = java.util.concurrent.atomic.AtomicInteger()
        val site = challengeServer(java.util.concurrent.atomic.AtomicBoolean(), java.util.Collections.synchronizedList(mutableListOf()))
        val solver = com.sun.net.httpserver.HttpServer.create(java.net.InetSocketAddress("127.0.0.1", 0), 0)
        solver.createContext("/v1") { ex ->
            solves.incrementAndGet()
            val body = """{"status":"error","message":"Cloudflare has blocked this request"}""".toByteArray()
            ex.responseHeaders.add("Content-Type", "application/json")
            ex.sendResponseHeaders(500, body.size.toLong())
            ex.responseBody.use { it.write(body) }
            ex.close()
        }
        solver.start()
        try {
            val ctx = JvmLoaderContext(FlareSolverr("http://127.0.0.1:${solver.address.port}", 2_000))
            val client = okhttp3.OkHttpClient.Builder().addInterceptor(ctx.cloudflareGuard).build()
            val url = "http://127.0.0.1:${site.address.port}/x"
            repeat(3) {
                assertFailsWith<CloudflareBlocked> {
                    client.newCall(okhttp3.Request.Builder().url(url).build()).execute()
                }
            }
            assertEquals(1, solves.get(), "blocked host must back off instead of re-solving")
        } finally {
            site.stop(0)
            solver.stop(0)
        }
    }

    @Test
    fun solverFailureReportsCloudflareBlocked() {
        val site = challengeServer(java.util.concurrent.atomic.AtomicBoolean(), java.util.Collections.synchronizedList(mutableListOf()))
        try {
            val ctx = JvmLoaderContext(FlareSolverr("http://127.0.0.1:1", 1_000))
            val client = okhttp3.OkHttpClient.Builder().addInterceptor(ctx.cloudflareGuard).build()
            assertFailsWith<CloudflareBlocked> {
                client.newCall(okhttp3.Request.Builder().url("http://127.0.0.1:${site.address.port}/x").build()).execute()
            }
        } finally {
            site.stop(0)
        }
    }

    private fun challengeServer(
        cleared: java.util.concurrent.atomic.AtomicBoolean,
        agents: MutableList<String>,
    ): com.sun.net.httpserver.HttpServer {
        val server = com.sun.net.httpserver.HttpServer.create(java.net.InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/x") { ex ->
            agents.add(ex.requestHeaders.getFirst("User-Agent").orEmpty())
            if (ex.requestHeaders.getFirst("Cookie")?.contains("cf_clearance") == true) cleared.set(true)
            if (cleared.get()) {
                ex.sendResponseHeaders(204, -1)
            } else {
                val body = """<html><body><div id="challenge-error-text">Just a moment</div></body></html>""".toByteArray()
                ex.sendResponseHeaders(403, body.size.toLong())
                ex.responseBody.use { it.write(body) }
            }
            ex.close()
        }
        server.start()
        return server
    }

    private fun solverServer(
        solves: java.util.concurrent.atomic.AtomicInteger,
        sitePort: Int,
    ): com.sun.net.httpserver.HttpServer {
        val server = com.sun.net.httpserver.HttpServer.create(java.net.InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/v1") { ex ->
            solves.incrementAndGet()
            val body = """{"status":"ok","solution":{"userAgent":"solved-agent","cookies":[
                {"name":"cf_clearance","value":"ok","domain":"127.0.0.1","path":"/","expiry":4102444800}]}}""".toByteArray()
            ex.responseHeaders.add("Content-Type", "application/json")
            ex.sendResponseHeaders(200, body.size.toLong())
            ex.responseBody.use { it.write(body) }
            ex.close()
        }
        server.start()
        return server
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
