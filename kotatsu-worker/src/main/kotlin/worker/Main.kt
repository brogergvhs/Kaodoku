package worker

import io.javalin.Javalin
import io.javalin.http.HttpResponseException
import io.javalin.http.UnauthorizedResponse
import org.koitharu.kotatsu.parsers.config.ConfigKey
import org.koitharu.kotatsu.parsers.model.ContentType
import org.koitharu.kotatsu.parsers.model.MangaParserSource
import java.io.File

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

class SourceCatalog(val sources: List<SourceInfo>, val hostIndex: Map<String, MangaParserSource>)

fun loadCatalog(ctx: JvmLoaderContext): SourceCatalog {
    val index = HashMap<String, MangaParserSource>(4096)
    val sources = MangaParserSource.entries.mapNotNull { s ->
        runCatching {
            val parser = ctx.newParserInstance(s)
            val keys = ArrayList<ConfigKey<*>>()
            parser.onCreateConfig(keys)
            val domains = parser.configKeyDomain.presetValues.toList()
            for (d in domains) {
                val host = d.removePrefix("www.")
                val cur = index[host]
                if (cur == null || (cur.isBroken && !s.isBroken)) index[host] = s
            }
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
                domains = domains,
                base_url = "https://${parser.domain}",
            )
        }.onFailure { log.warn("source {} failed to load: {}", s.name, it.toString()) }.getOrNull()
    }.sortedBy { it.name.lowercase() }
    return SourceCatalog(sources, index)
}

data class ConfigReq(val source: String, val values: Map<String, String> = emptyMap())

// cgroup v2 working set as docker stats reports it: usage minus inactive page cache.
fun containerMemory(): Long? = runCatching {
    var used = File("/sys/fs/cgroup/memory.current").readText().trim().toLong()
    File("/sys/fs/cgroup/memory.stat").forEachLine { line ->
        if (line.startsWith("inactive_file ")) used -= minOf(line.substringAfter(' ').trim().toLong(), used)
    }
    used
}.getOrNull()

fun main() {
    val key = System.getenv("WORKER_KEY")?.takeIf { it.isNotBlank() } ?: error("WORKER_KEY env is required")
    val port = System.getenv("PORT")?.toIntOrNull() ?: 8484
    val solverUrl = System.getenv("FLARESOLVERR_URL")?.takeIf { it.isNotBlank() }
    val solverTimeout = System.getenv("FLARESOLVERR_TIMEOUT_MS")?.toLongOrNull() ?: 60_000
    val browserUrl = System.getenv("BROWSER_WORKER_URL")?.takeIf { it.isNotBlank() }
    val ctx = JvmLoaderContext(solverUrl?.let { FlareSolverr(it, solverTimeout) }, browserUrl?.let { BrowserJs(it) })
    val catalog = loadCatalog(ctx)
    val sources = catalog.sources
    val api = Api(ctx, key, catalog.hostIndex)
    val open = setOf("/healthz", "/image")

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
                    "js" to ctx.jsEnabled,
                    "memory_bytes" to containerMemory(),
                ),
            )
        }
        .get("/sources") { c -> c.json(sources) }
        .post("/config") { c ->
            val req = c.bodyAsClass(ConfigReq::class.java)
            val src = runCatching { MangaParserSource.valueOf(req.source) }.getOrElse {
                throw io.javalin.http.NotFoundResponse("unknown source")
            }
            ctx.setConfig(src, req.values)
            c.status(204)
        }
        .get("/search", api::search)
        .get("/chapters", api::chapters)
        .get("/pages", api::pages)
        .get("/image", api::image)
        .start(port)
}
