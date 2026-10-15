package worker

import io.javalin.http.Context
import io.javalin.http.HttpResponseException
import org.jsoup.HttpStatusException
import org.koitharu.kotatsu.parsers.exception.AuthRequiredException
import org.koitharu.kotatsu.parsers.exception.ContentUnavailableException
import org.koitharu.kotatsu.parsers.exception.NotFoundException
import org.koitharu.kotatsu.parsers.exception.ParseException
import org.koitharu.kotatsu.parsers.exception.TooManyRequestExceptions
import org.slf4j.LoggerFactory
import java.io.IOException
import java.io.InterruptedIOException

class CloudflareBlocked(val url: String) : IOException("cloudflare challenge at $url")

class JsUnsupported : IOException("JS evaluation is not available")

data class ApiError(
    val error: String,
    val message: String,
    val source: String? = null,
    val url: String? = null,
    val upstream_status: Int? = null,
)

internal val log = LoggerFactory.getLogger("worker")

internal fun errorFor(e: Exception): Pair<Int, ApiError> = when (e) {
    is CloudflareBlocked -> 503 to ApiError("cloudflare_challenge", msg(e), url = e.url)
    is HttpResponseException -> e.status to ApiError(
        when (e.status) {
            400 -> "bad_request"
            401 -> "unauthorized"
            404 -> "not_found"
            429 -> "rate_limited"
            502 -> "upstream_http"
            else -> "http_${e.status}"
        },
        msg(e),
    )
    is NotFoundException -> 404 to ApiError("not_found", msg(e))
    is TooManyRequestExceptions -> 429 to ApiError("rate_limited", msg(e), url = e.url)
    is AuthRequiredException -> 401 to ApiError("auth_required", msg(e))
    is ContentUnavailableException -> 410 to ApiError("content_unavailable", msg(e))
    is ParseException -> 502 to ApiError("parse_failed", msg(e))
    is HttpStatusException -> 502 to ApiError("upstream_http", msg(e), url = e.url, upstream_status = e.statusCode)
    is JsUnsupported, is UnsupportedOperationException -> 501 to ApiError("unsupported", msg(e))
    is InterruptedIOException -> 504 to ApiError("upstream_timeout", msg(e))
    is IOException -> 502 to ApiError("upstream_io", msg(e))
    else -> 500 to ApiError("internal", msg(e))
}

private fun msg(e: Exception): String =
    e.message?.takeIf { it.isNotBlank() } ?: e::class.simpleName ?: "error"

fun handleError(e: Exception, c: Context) {
    val (status, body) = errorFor(e)
    c.status(status).json(body.copy(source = c.queryParam("source")))
    if (body.error == "internal") log.warn("${c.path()} failed", e)
    else log.warn("{} {} -> {} {}", c.path(), c.queryParam("source") ?: "-", body.error, body.message)
}
