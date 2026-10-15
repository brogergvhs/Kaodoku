@file:OptIn(InternalParsersApi::class)

package worker

import io.javalin.http.BadGatewayResponse
import io.javalin.http.BadRequestResponse
import io.javalin.http.Context
import io.javalin.http.NotFoundResponse
import io.javalin.http.UnauthorizedResponse
import kotlinx.coroutines.runBlocking
import okhttp3.Request
import org.koitharu.kotatsu.parsers.InternalParsersApi
import org.koitharu.kotatsu.parsers.MangaParser
import org.koitharu.kotatsu.parsers.model.Manga
import org.koitharu.kotatsu.parsers.model.MangaListFilter
import org.koitharu.kotatsu.parsers.model.MangaPage
import org.koitharu.kotatsu.parsers.model.MangaParserSource
import org.koitharu.kotatsu.parsers.model.MangaSource
import org.koitharu.kotatsu.parsers.model.SortOrder
import org.koitharu.kotatsu.parsers.util.generateUid
import java.net.URLEncoder
import java.util.Collections
import java.security.MessageDigest
import java.time.Instant
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec

data class SearchResult(val url: String, val title: String, val cover: String?, val alt_titles: List<String>)
data class ChapterInfo(
    val url: String, val number: String, val title: String,
    val volume: Int, val branch: String, val uploaded_at: String,
)
data class ChaptersResult(val resolved_url: String, val chapters: List<ChapterInfo>)

class Api(private val ctx: JvmLoaderContext, private val key: String) {
    private val mangaCache = Collections.synchronizedMap(
        object : LinkedHashMap<String, Manga>(16, 0.75f, true) {
            override fun removeEldestEntry(e: MutableMap.MutableEntry<String, Manga>?) = size > 64
        })

    private fun parser(id: String?): MangaParser {
        val s = id?.let { runCatching { MangaParserSource.valueOf(it) }.getOrNull() }
            ?: throw NotFoundResponse("unknown source")
        return ctx.parser(s)
    }

    private fun order(p: MangaParser, preferred: SortOrder) =
        if (preferred in p.availableSortOrders) preferred else p.availableSortOrders.first()

    fun search(c: Context) {
        val p = parser(c.queryParam("source"))
        val q = c.queryParam("q").orEmpty()
        val (filter, sort) =
            if (q.isBlank()) MangaListFilter() to SortOrder.POPULARITY
            else MangaListFilter(query = q) to SortOrder.RELEVANCE
        val list = runBlocking { p.getList(0, order(p, sort), filter) }
        c.json(list.take(20).map { SearchResult(it.publicUrl, it.title, it.coverUrl, it.altTitles.toList()) })
    }

    private fun resolve(p: MangaParser, publicUrl: String): Manga =
        mangaCache.getOrPut("${p.source.name}|$publicUrl") {
            runBlocking {
                val resolved = ctx.newLinkResolver(publicUrl).getManga()
                    ?: throw NotFoundResponse("manga not found on source")
                val manga = if (resolved.source == p.source) resolved
                else resolved.copy(source = p.source, chapters = null)
                if (manga.chapters.isNullOrEmpty()) p.getDetails(manga) else manga
            }
        }

    fun chapters(c: Context) {
        val p = parser(c.queryParam("source"))
        val url = c.queryParam("url") ?: throw BadRequestResponse("url required")
        mangaCache.remove("${p.source.name}|$url")
        val manga = resolve(p, url)
        mangaCache["${p.source.name}|${manga.publicUrl}"] = manga
        c.json(ChaptersResult(manga.publicUrl, manga.chapters.orEmpty().map {
            ChapterInfo(it.url, it.numberString() ?: "", it.title ?: "", it.volume,
                it.branch ?: "", if (it.uploadDate > 0) Instant.ofEpochMilli(it.uploadDate).toString() else "")
        }))
    }

    fun pages(c: Context) {
        val p = parser(c.queryParam("source"))
        val mangaUrl = c.queryParam("manga") ?: throw BadRequestResponse("manga required")
        val chapterUrl = c.queryParam("chapter") ?: throw BadRequestResponse("chapter required")
        val chapter = resolve(p, mangaUrl).chapters.orEmpty().firstOrNull { it.url == chapterUrl }
            ?: throw NotFoundResponse("chapter not found")
        val pages = runBlocking { p.getPages(chapter) }
        c.json(pages.map { mapOf("image" to imagePath(p.source.name, it.url)) })
    }

    fun image(c: Context) {
        val src = c.queryParam("source") ?: throw BadRequestResponse("source required")
        val url = c.queryParam("url") ?: throw BadRequestResponse("url required")
        if (!verify(c.queryParam("sig"), src, url)) throw UnauthorizedResponse()
        val p = parser(src)
        val direct = runBlocking { p.getPageUrl(MangaPage(p.generateUid(url), url, null, p.source)) }
        val req = runCatching {
            Request.Builder().url(direct)
                .tag(MangaSource::class.java, p.source)
                .header("User-Agent", ctx.getDefaultUserAgent())
                .header("Referer", "https://${p.domain}/")
                .build()
        }.getOrElse { throw BadRequestResponse("bad image url") }
        val resp = ctx.httpClient.newCall(req).execute()
        if (!resp.isSuccessful) {
            resp.close()
            throw BadGatewayResponse("upstream ${resp.code}")
        }
        c.contentType(resp.header("Content-Type") ?: "image/jpeg").result(resp.body!!.byteStream())
    }

    internal fun sign(source: String, url: String): String {
        val mac = Mac.getInstance("HmacSHA256")
        mac.init(SecretKeySpec(key.toByteArray(), "HmacSHA256"))
        return mac.doFinal("$source\n$url".toByteArray()).joinToString("") { "%02x".format(it) }
    }

    internal fun verify(sig: String?, source: String, url: String) =
        sig != null && MessageDigest.isEqual(sig.toByteArray(), sign(source, url).toByteArray())

    internal fun imagePath(source: String, url: String): String {
        val e = { s: String -> URLEncoder.encode(s, "UTF-8") }
        return "/image?source=${e(source)}&url=${e(url)}&sig=${sign(source, url)}"
    }
}
