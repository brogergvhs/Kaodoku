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

class JvmLoaderContext : MangaLoaderContext() {
    override val cookieJar: CookieJar = MemoryCookieJar()

    private val parsers = ConcurrentHashMap<MangaParserSource, MangaParser>()

    internal fun parser(source: MangaParserSource): MangaParser = parsers.getOrPut(source) { newParserInstance(source) }

    internal val cloudflareGuard = Interceptor { chain ->
        val response = chain.proceed(chain.request())
        if (CloudFlareHelper.checkResponseForProtection(response) == CloudFlareHelper.PROTECTION_NOT_DETECTED) {
            response
        } else {
            response.close()
            throw CloudflareBlocked(chain.request().url.toString())
        }
    }

    internal val parserDispatch = Interceptor { chain ->
        val source = chain.request().tag(MangaSource::class.java)
        if (source is MangaParserSource) parser(source).intercept(chain) else chain.proceed(chain.request())
    }

    override val httpClient: OkHttpClient = OkHttpClient.Builder()
        .cookieJar(cookieJar)
        .addInterceptor(cloudflareGuard)
        .addInterceptor(parserDispatch)
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
