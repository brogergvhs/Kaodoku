package worker

import io.javalin.Javalin
import io.javalin.http.HttpResponseException
import io.javalin.http.UnauthorizedResponse
import org.koitharu.kotatsu.parsers.config.ConfigKey
import org.koitharu.kotatsu.parsers.model.ContentType
import org.koitharu.kotatsu.parsers.model.MangaParserSource

data class SourceInfo(
    val id: String,
    val name: String,
    val language: String,
    val content_type: String,
    val nsfw: Boolean,
    val broken: Boolean,
    val cloudflare: Boolean,
    val searchable: Boolean,
    val domains: List<String>,
    val base_url: String,
)

private val nsfwHint = Regex("hentai|porn|adult|doujin|yaoi", RegexOption.IGNORE_CASE)

fun listSources(ctx: JvmLoaderContext): List<SourceInfo> =
    MangaParserSource.entries.mapNotNull { s ->
        runCatching {
            val parser = ctx.newParserInstance(s)
            val keys = ArrayList<ConfigKey<*>>()
            parser.onCreateConfig(keys)
            SourceInfo(
                id = s.name,
                name = s.title,
                language = s.locale,
                content_type = s.contentType.name.lowercase(),
                nsfw = s.contentType == ContentType.HENTAI ||
                    nsfwHint.containsMatchIn(s.name) || nsfwHint.containsMatchIn(parser.domain),
                broken = s.isBroken,
                cloudflare = keys.any { it.key == "intercept_cloudflare" },
                searchable = parser.filterCapabilities.isSearchSupported,
                domains = parser.configKeyDomain.presetValues.toList(),
                base_url = "https://${parser.domain}",
            )
        }.getOrNull()
    }.sortedBy { it.name.lowercase() }

fun main() {
    val key = System.getenv("WORKER_KEY")?.takeIf { it.isNotBlank() } ?: error("WORKER_KEY env is required")
    val port = System.getenv("PORT")?.toIntOrNull() ?: 8484
    val solverUrl = System.getenv("FLARESOLVERR_URL")?.takeIf { it.isNotBlank() }
    val solverTimeout = System.getenv("FLARESOLVERR_TIMEOUT_MS")?.toLongOrNull() ?: 60_000
    val ctx = JvmLoaderContext(solverUrl?.let { FlareSolverr(it, solverTimeout) })
    val api = Api(ctx, key)
    val open = setOf("/healthz", "/image")
    val sources = listSources(ctx)

    Javalin.create()
        .exception(HttpResponseException::class.java, ::handleError)
        .exception(Exception::class.java, ::handleError)
        .before { c ->
            if (c.path() !in open && c.header("X-Worker-Key") != key) throw UnauthorizedResponse()
        }
        .get("/healthz") { c ->
            c.json(
                mapOf(
                    "ok" to true,
                    "parsers" to sources.size,
                    "version" to System.getProperty("worker.lib", "dev"),
                    "solver" to ctx.solverEnabled,
                ),
            )
        }
        .get("/sources") { c -> c.json(sources) }
        .get("/search", api::search)
        .get("/chapters", api::chapters)
        .get("/pages", api::pages)
        .get("/image", api::image)
        .start(port)
}
