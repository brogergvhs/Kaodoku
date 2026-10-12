package worker

import io.javalin.http.BadRequestResponse
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class ApiTest {
    private val api = Api(JvmLoaderContext(), "test-key")

    @Test
    fun signatureRoundTrip() {
        val sig = api.sign("MANGADEX", "https://x/img.jpg")
        assertTrue(api.verify(sig, "MANGADEX", "https://x/img.jpg"))
        assertFalse(api.verify(sig, "MANGADEX", "https://x/other.jpg"))
        assertFalse(api.verify(sig, "OTHER", "https://x/img.jpg"))
        assertFalse(api.verify(null, "MANGADEX", "https://x/img.jpg"))
        assertFalse(api.verify("", "MANGADEX", "https://x/img.jpg"))
    }

    @Test
    fun differentKeysDiffer() {
        val other = Api(JvmLoaderContext(), "other-key")
        assertFalse(other.verify(api.sign("S", "u"), "S", "u"))
    }

    @Test
    fun imagePathEncodesAndVerifies() {
        val path = api.imagePath("MANGADEX", "https://cdn.x/a b.jpg?v=1&k=2")
        assertTrue(path.startsWith("/image?source=MANGADEX&url=https%3A%2F%2Fcdn.x%2Fa+b.jpg%3Fv%3D1%26k%3D2&sig="))
        val sig = path.substringAfter("sig=")
        assertTrue(api.verify(sig, "MANGADEX", "https://cdn.x/a b.jpg?v=1&k=2"))
    }

    @Test
    fun privateAddressGuardBlocksLoopback() {
        val server = com.sun.net.httpserver.HttpServer.create(java.net.InetSocketAddress("127.0.0.1", 0), 0)
        server.createContext("/x") { ex -> ex.sendResponseHeaders(200, 0); ex.close() }
        server.start()
        try {
            val call = JvmLoaderContext().httpClient.newCall(
                okhttp3.Request.Builder().url("http://127.0.0.1:${server.address.port}/x").build()
            )
            assertFailsWith<java.io.IOException> { call.execute() }
        } finally {
            server.stop(0)
        }
    }
}
