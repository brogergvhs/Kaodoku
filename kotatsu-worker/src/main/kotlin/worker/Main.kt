package worker

import io.javalin.Javalin
import io.javalin.http.UnauthorizedResponse
import org.koitharu.kotatsu.parsers.model.ContentType
import org.koitharu.kotatsu.parsers.model.MangaParserSource

data class SourceInfo(
    val id: String,
    val name: String,
    val language: String,
    val content_type: String,
    val nsfw: Boolean,
    val broken: Boolean,
    val domains: List<String>,
    val base_url: String,
)

fun listSources(ctx: JvmLoaderContext): List<SourceInfo> =
    MangaParserSource.entries.mapNotNull { s ->
        runCatching {
            val parser = ctx.newParserInstance(s)
            SourceInfo(
                id = s.name,
                name = s.title,
                language = s.locale,
                content_type = s.contentType.name.lowercase(),
                nsfw = s.contentType == ContentType.HENTAI,
                broken = s.isBroken,
                domains = parser.configKeyDomain.presetValues.toList(),
                base_url = "https://${parser.domain}",
            )
        }.getOrNull()
    }.sortedBy { it.name.lowercase() }

fun main() {
    val key = System.getenv("WORKER_KEY") ?: error("WORKER_KEY env is required")
    val port = System.getenv("PORT")?.toIntOrNull() ?: 8484
    val ctx = JvmLoaderContext()
    val sources by lazy { listSources(ctx) }

    Javalin.create()
        .before { c ->
            if (c.path() != "/healthz" && c.header("X-Worker-Key") != key) throw UnauthorizedResponse()
        }
        .get("/healthz") { c ->
            c.json(mapOf("ok" to true, "parsers" to sources.size, "version" to System.getProperty("worker.lib", "dev")))
        }
        .get("/sources") { c -> c.json(sources) }
        .start(port)
}
