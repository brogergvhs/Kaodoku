package worker

import okhttp3.Cookie
import okhttp3.CookieJar
import okhttp3.HttpUrl
import okhttp3.Interceptor
import okhttp3.OkHttpClient
import okhttp3.Response
import org.koitharu.kotatsu.parsers.MangaLoaderContext
import org.koitharu.kotatsu.parsers.MangaParser
import org.koitharu.kotatsu.parsers.bitmap.Bitmap
import org.koitharu.kotatsu.parsers.config.ConfigKey
import org.koitharu.kotatsu.parsers.config.MangaSourceConfig
import org.koitharu.kotatsu.parsers.model.MangaParserSource
import org.koitharu.kotatsu.parsers.model.MangaSource
import org.koitharu.kotatsu.parsers.network.CloudFlareHelper
import java.io.IOException
import java.util.concurrent.ConcurrentHashMap
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

class JvmLoaderContext(private val solver: FlareSolverr? = null) : MangaLoaderContext() {
    override val cookieJar: CookieJar = MemoryCookieJar()

    internal val solverEnabled = solver != null

    private val parsers = ConcurrentHashMap<MangaParserSource, MangaParser>()
    private val solveLocks = ConcurrentHashMap<String, Any>()
    private val solveFailedUntil = ConcurrentHashMap<String, Long>()
    private val solveFails = ConcurrentHashMap<String, Int>()
    private val solvedAt = ConcurrentHashMap<String, Long>()
    private val solvedAgents = ConcurrentHashMap<String, String>()

    internal fun parser(source: MangaParserSource): MangaParser = parsers.getOrPut(source) { newParserInstance(source) }

    // peekBody caps the read: the helper itself peeks Long.MAX_VALUE.
    private fun isChallenge(response: Response): Boolean {
        if (response.code != 403 && response.code != 503) return false
        val probe = response.newBuilder().body(response.peekBody(256_000)).build()
        return CloudFlareHelper.checkResponseForProtection(probe) != CloudFlareHelper.PROTECTION_NOT_DETECTED
    }

    // Hosts that can never be solved (banned IP, protected CDN) back off to 30 min.
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

    // IOException subtype, not UnsupportedOperationException: parsers call this inside interceptors.
    @Deprecated("Provide a base url")
    override suspend fun evaluateJs(script: String): String? = throw JsUnsupported()

    override suspend fun evaluateJs(baseUrl: String, script: String, timeout: Long): String? = throw JsUnsupported()

    override fun getConfig(source: MangaSource): MangaSourceConfig = DefaultConfig

    override fun getDefaultUserAgent(): String = UA

    override fun redrawImageResponse(response: Response, redraw: (Bitmap) -> Bitmap): Response =
        throw UnsupportedOperationException("Image descrambling is not available")

    override fun createBitmap(width: Int, height: Int): Bitmap =
        throw UnsupportedOperationException("Bitmap is not available")

    private object DefaultConfig : MangaSourceConfig {
        override fun <T> get(key: ConfigKey<T>): T = key.defaultValue
    }
}
