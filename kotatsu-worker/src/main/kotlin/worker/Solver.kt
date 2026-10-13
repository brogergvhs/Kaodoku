package worker

import com.fasterxml.jackson.databind.JsonNode
import com.fasterxml.jackson.module.kotlin.jacksonObjectMapper
import okhttp3.Cookie
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
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
