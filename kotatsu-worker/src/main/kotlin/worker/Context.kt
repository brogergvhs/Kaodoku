package worker

import okhttp3.Cookie
import okhttp3.CookieJar
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.Interceptor
import okhttp3.OkHttpClient
import okhttp3.Response
import org.koitharu.kotatsu.parsers.MangaLoaderContext
import org.koitharu.kotatsu.parsers.MangaParser
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.ResponseBody.Companion.toResponseBody
import org.koitharu.kotatsu.parsers.bitmap.Bitmap
import org.koitharu.kotatsu.parsers.bitmap.Rect
import org.koitharu.kotatsu.parsers.config.ConfigKey
import org.koitharu.kotatsu.parsers.config.MangaSourceConfig
import org.koitharu.kotatsu.parsers.model.MangaParserSource
import org.koitharu.kotatsu.parsers.model.MangaSource
import org.koitharu.kotatsu.parsers.network.CloudFlareHelper
import org.koitharu.kotatsu.parsers.webview.InterceptedRequest
import org.koitharu.kotatsu.parsers.webview.InterceptionConfig
import java.awt.image.BufferedImage
import java.io.ByteArrayOutputStream
import java.io.IOException
import javax.imageio.IIOImage
import javax.imageio.ImageIO
import javax.imageio.ImageWriteParam
import javax.imageio.stream.MemoryCacheImageOutputStream
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Semaphore
import java.util.concurrent.TimeUnit

private const val UA =
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Safari/537.36"

class MemoryCookieJar : CookieJar {
    private val store = ConcurrentHashMap<String, Cookie>()

    override fun saveFromResponse(url: HttpUrl, cookies: List<Cookie>) {
        val now = System.currentTimeMillis()
        store.values.removeIf { it.expiresAt <= now }
        for (c in cookies) store["${c.domain}|${c.name}|${c.path}"] = c
    }

    override fun loadForRequest(url: HttpUrl): List<Cookie> {
        val now = System.currentTimeMillis()
        return store.values.filter { it.expiresAt > now && it.matches(url) }
    }
}

private val privateAddressGuard = okhttp3.Interceptor { chain ->
    val addr = (chain.connection()?.route()?.socketAddress as? java.net.InetSocketAddress)?.address
    if (addr != null && (addr.isLoopbackAddress || addr.isSiteLocalAddress ||
            addr.isLinkLocalAddress || addr.isAnyLocalAddress)
    ) throw java.io.IOException("refused private address ${addr.hostAddress}")
    chain.proceed(chain.request())
}

class JvmLoaderContext(
    private val solver: FlareSolverr? = null,
    private val js: BrowserJs? = null,
) : MangaLoaderContext() {
    override val cookieJar: CookieJar = MemoryCookieJar()

    internal val solverEnabled = solver != null
    internal val jsEnabled = js != null

    private val parsers = ConcurrentHashMap<MangaParserSource, MangaParser>()
    private val configs = ConcurrentHashMap<String, Map<String, String>>()
    private val solveLocks = ConcurrentHashMap<String, Any>()
    private val solveFailedUntil = ConcurrentHashMap<String, Long>()
    private val solveFails = ConcurrentHashMap<String, Int>()
    private val solvedAt = ConcurrentHashMap<String, Long>()
    private val solvedAgents = ConcurrentHashMap<String, String>()

    internal fun parser(source: MangaParserSource): MangaParser = parsers.getOrPut(source) { newParserInstance(source) }

    internal fun setConfig(source: MangaParserSource, values: Map<String, String>) {
        if (configs[source.name] == values) return
        if (values.isEmpty()) configs.remove(source.name) else configs[source.name] = values
        parsers.remove(source)
    }

    private fun isChallenge(response: Response): Boolean {
        if (response.code != 403 && response.code != 503) return false
        val probe = response.newBuilder().body(response.peekBody(256_000)).build()
        return CloudFlareHelper.checkResponseForProtection(probe) != CloudFlareHelper.PROTECTION_NOT_DETECTED
    }

    private fun backOff(host: String) {
        val fails = solveFails.merge(host, 1, Int::plus) ?: 1
        solveFailedUntil[host] = System.currentTimeMillis() + minOf(60_000L shl minOf(fails - 1, 5), 1_800_000L)
    }

    private fun solve(url: HttpUrl, since: Long) {
        val host = url.host
        synchronized(solveLocks.getOrPut(host) { Any() }) {
            if ((solvedAt[host] ?: 0) > since) return
            if (System.currentTimeMillis() < (solveFailedUntil[host] ?: 0)) throw CloudflareBlocked(url.toString())
            val started = System.currentTimeMillis()
            log.info("solving cloudflare challenge for {}", host)
            val solution = try {
                solver!!.solve(url.toString())
            } catch (e: IOException) {
                backOff(host)
                log.warn("flaresolverr failed for {} after {}ms: {}", host, System.currentTimeMillis() - started, e.message)
                throw CloudflareBlocked(url.toString())
            }
            log.info("solved {} in {}ms", host, System.currentTimeMillis() - started)
            cookieJar.saveFromResponse(url, solution.cookies)
            if (solution.userAgent.isNotBlank()) solvedAgents[agentKey(url)] = solution.userAgent
            solvedAt[host] = System.currentTimeMillis()
            solveFailedUntil.remove(host)
            solveFails.remove(host)
        }
    }

    internal val cloudflareGuard = Interceptor { chain ->
        val response = chain.proceed(chain.request())
        if (!isChallenge(response)) return@Interceptor response
        val url = chain.request().url
        if (solver == null) {
            response.close()
            throw CloudflareBlocked(url.toString())
        }
        val since = solvedAt[url.host] ?: 0
        response.close()
        solve(url, since)
        val retry = chain.proceed(chain.request())
        if (!isChallenge(retry)) return@Interceptor retry
        retry.close()
        backOff(url.host)
        throw CloudflareBlocked(url.toString())
    }

    internal val parserDispatch = Interceptor { chain ->
        val source = chain.request().tag(MangaSource::class.java)
        if (source is MangaParserSource) parser(source).intercept(chain) else chain.proceed(chain.request())
    }

    private fun agentKey(url: HttpUrl) = url.topPrivateDomain() ?: url.host

    internal val solvedAgentPin = Interceptor { chain ->
        val agent = solvedAgents[agentKey(chain.request().url)]
        if (agent == null) chain.proceed(chain.request())
        else chain.proceed(chain.request().newBuilder().header("User-Agent", agent).build())
    }

    override val httpClient: OkHttpClient = OkHttpClient.Builder()
        .cookieJar(cookieJar)
        .addInterceptor(cloudflareGuard)
        .addInterceptor(parserDispatch)
        .addInterceptor(solvedAgentPin)
        .addNetworkInterceptor(privateAddressGuard)
        .connectTimeout(20, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .build()

    @Deprecated("Provide a base url")
    override suspend fun evaluateJs(script: String): String? = evaluateJs("about:blank", script, 60_000)

    override suspend fun evaluateJs(baseUrl: String, script: String, timeout: Long): String? {
        val browser = js ?: throw JsUnsupported()
        return withContext(Dispatchers.IO) {
            withBrowser(baseUrl) {
                browser.evaluate(baseUrl, script, if (timeout > 0) timeout else 60_000, session(baseUrl))
            }
        }
    }

    // Selenium sessions get a fresh FlareSolverr clearance: minutes-old
    // cookies that still satisfy OkHttp are rejected by Cloudflare when a new
    // browser presents them, and a block inside the page retries once more.
    private fun <T> withBrowser(url: String, block: () -> T): T {
        val parsed = url.toHttpUrlOrNull()
        if (solver != null && parsed != null) {
            val last = solvedAt[parsed.host]
            if (last != null && System.currentTimeMillis() - last > 60_000) {
                solve(parsed, last)
            }
        }
        return try {
            block()
        } catch (e: IOException) {
            if (solver == null || parsed == null || e.message?.contains("blocked by Cloudflare") != true) throw e
            solve(parsed, solvedAt[parsed.host] ?: 0)
            block()
        }
    }

    // Hand the FlareSolverr clearance to the Selenium session: cf_clearance is
    // bound to the user agent it was solved with.
    private fun session(url: String): BrowserSession {
        val parsed = url.toHttpUrlOrNull() ?: return BrowserSession(emptyList(), null)
        return BrowserSession(cookieJar.loadForRequest(parsed), solvedAgents[agentKey(parsed)])
    }

    override suspend fun interceptWebViewRequests(url: String, urlPattern: String, timeoutMs: Long): List<InterceptedRequest> =
        interceptWebViewRequests(url, InterceptionConfig(timeoutMs = timeoutMs, urlPattern = Regex(urlPattern)))

    override suspend fun interceptWebViewRequests(url: String, config: InterceptionConfig): List<InterceptedRequest> {
        val browser = js ?: throw UnsupportedOperationException("WebView request interception is not available")
        return withContext(Dispatchers.IO) {
            withBrowser(url) { browser.intercept(url, config, session(url)) }
        }
    }

    override suspend fun captureWebViewUrls(url: String, urlPattern: Regex, timeoutMs: Long): List<String> {
        val browser = js ?: throw UnsupportedOperationException("WebView URL capture is not available")
        return withContext(Dispatchers.IO) {
            withBrowser(url) {
                browser.intercept(url, InterceptionConfig(timeoutMs = timeoutMs, urlPattern = urlPattern), session(url))
                    .map { it.url }
            }
        }
    }

    override fun getConfig(source: MangaSource): MangaSourceConfig = object : MangaSourceConfig {
        @Suppress("UNCHECKED_CAST")
        override fun <T> get(key: ConfigKey<T>): T {
            val raw = configs[source.name]?.get(key.key) ?: return key.defaultValue
            return when (key.defaultValue) {
                is Boolean -> (raw.toBooleanStrictOrNull() ?: key.defaultValue) as T
                else -> raw as T
            }
        }
    }

    override fun getDefaultUserAgent(): String = UA

    private val redrawPermits = Semaphore(2)

    // no local keeps the decoded source alive past redraw(), collectable before encode
    override fun redrawImageResponse(response: Response, redraw: (Bitmap) -> Bitmap): Response {
        redrawPermits.acquire()
        try {
            var hasAlpha = false
            val redrawn = (redraw(AwtBitmap(
                (response.body?.byteStream()?.use { ImageIO.read(it) }
                    ?: throw IOException("unsupported image format"))
                    .also { hasAlpha = it.colorModel.hasAlpha() }
            )) as AwtBitmap).image
            val usePng = hasAlpha || redrawn.colorModel.hasAlpha()
            val bytes = if (usePng) encodePng(redrawn) else encodeJpeg(redrawn)
            val type = if (usePng) "image/png" else "image/jpeg"
            return response.newBuilder()
                .body(bytes.toResponseBody(type.toMediaType()))
                .header("Content-Type", type)
                .build()
        } finally {
            redrawPermits.release()
        }
    }

    private fun encodePng(img: BufferedImage): ByteArray {
        val out = ByteArrayOutputStream()
        ImageIO.write(img, "png", out)
        return out.toByteArray()
    }

    private fun encodeJpeg(img: BufferedImage): ByteArray {
        val out = ByteArrayOutputStream(256 * 1024)
        val writer = ImageIO.getImageWritersByFormatName("jpeg").next()
        val param = writer.defaultWriteParam.apply {
            compressionMode = ImageWriteParam.MODE_EXPLICIT
            compressionQuality = 0.9f
        }
        MemoryCacheImageOutputStream(out).use { ios ->
            writer.output = ios
            writer.write(null, IIOImage(img, null, null), param)
        }
        writer.dispose()
        return out.toByteArray()
    }

    override fun createBitmap(width: Int, height: Int): Bitmap =
        AwtBitmap(BufferedImage(width, height, BufferedImage.TYPE_3BYTE_BGR))
}

internal class AwtBitmap(val image: BufferedImage) : Bitmap {
    override val width get() = image.width
    override val height get() = image.height

    override fun drawBitmap(sourceBitmap: Bitmap, src: Rect, dst: Rect) {
        val g = image.createGraphics()
        g.drawImage(
            (sourceBitmap as AwtBitmap).image,
            dst.left, dst.top, dst.right, dst.bottom,
            src.left, src.top, src.right, src.bottom,
            null,
        )
        g.dispose()
    }
}
