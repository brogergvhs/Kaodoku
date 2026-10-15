package worker

import com.fasterxml.jackson.databind.JsonNode
import com.fasterxml.jackson.module.kotlin.jacksonObjectMapper
import okhttp3.Cookie
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.koitharu.kotatsu.parsers.webview.InterceptedRequest
import org.koitharu.kotatsu.parsers.webview.InterceptionConfig
import java.io.IOException
import java.util.concurrent.TimeUnit

class Solution(val cookies: List<Cookie>, val userAgent: String)

class FlareSolverr(endpoint: String, private val timeoutMs: Long) {
    private val url = (endpoint.trimEnd('/').removeSuffix("/v1") + "/v1").toHttpUrl()
    private val json = jacksonObjectMapper()
    private val client = OkHttpClient.Builder()
        .callTimeout(timeoutMs + 15_000, TimeUnit.MILLISECONDS)
        .readTimeout(timeoutMs + 15_000, TimeUnit.MILLISECONDS)
        .build()

    fun solve(target: String): Solution {
        val payload = json.writeValueAsString(
            mapOf("cmd" to "request.get", "url" to target, "maxTimeout" to timeoutMs),
        )
        val req = Request.Builder().url(url)
            .post(payload.toRequestBody("application/json".toMediaType()))
            .build()
        client.newCall(req).execute().use { resp ->
            val body = resp.body?.byteStream() ?: throw IOException("flaresolverr: empty response")
            val root = json.readTree(body)
            if (!resp.isSuccessful || root["status"]?.asText() != "ok") {
                throw IOException("flaresolverr: ${root["message"]?.asText() ?: resp.code}")
            }
            val solution = root["solution"] ?: throw IOException("flaresolverr: no solution")
            return Solution(
                solution["cookies"].orEmpty().mapNotNull(::toCookie),
                solution["userAgent"]?.asText().orEmpty(),
            )
        }
    }
}

class BrowserSession(cookies: List<Cookie>, val userAgent: String?) {
    val cookies = cookies.map {
        mapOf("name" to it.name, "value" to it.value, "domain" to it.domain, "path" to it.path, "secure" to it.secure)
    }
}

class BrowserJs(endpoint: String) {
    private val url = (endpoint.trimEnd('/') + "/evaluate").toHttpUrl()
    private val json = jacksonObjectMapper()
    private val client = OkHttpClient.Builder()
        .callTimeout(180_000, TimeUnit.MILLISECONDS)
        .readTimeout(180_000, TimeUnit.MILLISECONDS)
        .build()

    fun intercept(pageUrl: String, config: InterceptionConfig, session: BrowserSession): List<InterceptedRequest> {
        val payload = json.writeValueAsString(
            mapOf(
                "page_url" to pageUrl,
                "url_pattern" to config.urlPattern?.pattern,
                "page_script" to config.pageScript,
                "timeout_ms" to config.timeoutMs,
                "max_requests" to config.maxRequests,
                "cookies" to session.cookies,
                "user_agent" to session.userAgent,
            ),
        )
        val req = Request.Builder().url(url.newBuilder().encodedPath("/intercept").build())
            .post(payload.toRequestBody("application/json".toMediaType()))
            .build()
        val call = client.newBuilder()
            .callTimeout(config.timeoutMs + 150_000, TimeUnit.MILLISECONDS)
            .readTimeout(config.timeoutMs + 150_000, TimeUnit.MILLISECONDS)
            .build()
        call.newCall(req).execute().use { resp ->
            val body = resp.body?.byteStream() ?: throw IOException("browser worker: empty response")
            val root = json.readTree(body)
            if (!resp.isSuccessful) {
                throw IOException("browser worker: ${root["error"]?.asText() ?: resp.code}")
            }
            val now = System.currentTimeMillis()
            return root["requests"].orEmpty().map { n ->
                InterceptedRequest(
                    n["url"]?.asText().orEmpty(),
                    n["method"]?.asText() ?: "GET",
                    n["headers"]?.fields()?.asSequence()?.associate { it.key to it.value.asText() }.orEmpty(),
                    now,
                    n["body"]?.takeUnless { it.isNull }?.asText(),
                )
            }
        }
    }

    fun evaluate(pageUrl: String, script: String, timeoutMs: Long, session: BrowserSession): String? {
        val payload = json.writeValueAsString(
            mapOf(
                "page_url" to pageUrl, "script" to script, "timeout_ms" to timeoutMs,
                "cookies" to session.cookies, "user_agent" to session.userAgent,
            ),
        )
        val req = Request.Builder().url(url)
            .post(payload.toRequestBody("application/json".toMediaType()))
            .build()
        client.newCall(req).execute().use { resp ->
            val body = resp.body?.byteStream() ?: throw IOException("browser worker: empty response")
            val root = json.readTree(body)
            if (!resp.isSuccessful) {
                throw IOException("browser worker: ${root["error"]?.asText() ?: resp.code}")
            }
            val node = root["result"]
            return if (node == null || node.isNull) null else node.toString()
        }
    }
}

private fun JsonNode?.orEmpty(): List<JsonNode> = this?.toList().orEmpty()

private fun toCookie(n: JsonNode): Cookie? {
    val name = n["name"]?.asText() ?: return null
    val value = n["value"]?.asText() ?: return null
    val domain = n["domain"]?.asText()?.removePrefix(".")?.takeIf { it.isNotBlank() } ?: return null
    val b = Cookie.Builder().name(name).value(value).domain(domain).path(n["path"]?.asText() ?: "/")
    val expires = (n["expiry"] ?: n["expires"])?.asLong() ?: 0L
    if (expires > 0) b.expiresAt(expires * 1000)
    if (n["secure"]?.asBoolean() == true) b.secure()
    return runCatching { b.build() }.getOrNull()
}
