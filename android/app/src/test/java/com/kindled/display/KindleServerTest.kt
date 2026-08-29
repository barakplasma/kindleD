package com.kindled.display

import java.io.BufferedInputStream
import java.io.InputStream
import java.io.OutputStream
import java.net.Socket
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

/**
 * Exercises the Pixel end of the protocol against a socket that behaves the
 * way kindled does. Both implementations are written from PROTOCOL.md, so
 * these tests are where the two readings of it get to disagree.
 */
class KindleServerTest {

    private lateinit var server: KindleServer
    private val events = mutableListOf<String>()
    private var negotiated = Pair(1072, 1448)

    private val listener = object : KindleServer.Listener {
        override fun onNegotiateGeometry(kindleWidth: Int, kindleHeight: Int): Pair<Int, Int> {
            synchronized(events) { events.add("negotiate $kindleWidth $kindleHeight") }
            return negotiated
        }

        override fun onKindleConnected(width: Int, height: Int) {
            synchronized(events) { events.add("connected $width $height") }
        }

        override fun onKindleDisconnected(reason: String) {
            synchronized(events) { events.add("disconnected") }
        }

        override fun onTap(x: Int, y: Int) {
            synchronized(events) { events.add("tap $x $y") }
        }

        override fun onScroll(dy: Int) {
            synchronized(events) { events.add("scroll $dy") }
        }
    }

    @Before
    fun setUp() {
        // Ephemeral port, and timings tightened so the tests are quick.
        server = KindleServer(
            port = 0,
            frameIntervalMs = 20,
            ackDeadlineMs = 80,
            listener = listener,
        )
        server.start()
        waitFor("listener to bind") { server.boundPort != 0 }
    }

    @After
    fun tearDown() {
        server.stop()
    }

    private fun waitFor(what: String, condition: () -> Boolean) {
        val deadline = System.currentTimeMillis() + 3000
        while (System.currentTimeMillis() < deadline) {
            if (condition()) return
            Thread.sleep(5)
        }
        throw AssertionError("timed out waiting for $what")
    }

    private fun events(): List<String> = synchronized(events) { events.toList() }

    /** A minimal kindled: dials in, says HELLO, reads control lines. */
    private inner class FakeKindle(
        width: Int = 1072,
        height: Int = 1448,
        hello: String = "HELLO kindled/1 $width $height",
    ) : AutoCloseable {
        val socket = Socket("127.0.0.1", server.boundPort)
        private val input: InputStream = BufferedInputStream(socket.getInputStream())
        private val output: OutputStream = socket.getOutputStream()

        init {
            socket.soTimeout = 3000
            send(hello)
        }

        fun send(line: String) {
            output.write("$line\n".toByteArray())
            output.flush()
        }

        fun readLine(): String {
            val sb = StringBuilder()
            while (true) {
                val b = input.read()
                if (b < 0) return sb.toString()
                if (b == '\n'.code) return sb.toString()
                sb.append(b.toChar())
            }
        }

        /** Reads a FRAME header and its payload, returning seq to bytes. */
        fun readFrame(): Pair<Long, ByteArray> {
            val header = readLine().split(" ")
            assertEquals("FRAME", header[0])
            val seq = header[1].toLong()
            val length = header[2].toInt()
            val payload = ByteArray(length)
            var read = 0
            while (read < length) {
                val n = input.read(payload, read, length - read)
                if (n < 0) break
                read += n
            }
            assertEquals(length, read)
            return Pair(seq, payload)
        }

        override fun close() {
            socket.close()
        }
    }

    @Test
    fun handshakeAnswersReadyWithNegotiatedGeometry() {
        negotiated = Pair(800, 1000)
        FakeKindle().use { kindle ->
            val ready = kindle.readLine().split(" ")
            assertEquals("READY", ready[0])
            assertEquals("800", ready[1])
            assertEquals("1000", ready[2])
            // fps is derived from the frame interval.
            assertEquals("50", ready[3])
            assertEquals(800, server.frameWidth)
            waitFor("connect callback") { events().contains("connected 1072 1448") }
            assertTrue(events().contains("negotiate 1072 1448"))
        }
    }

    /**
     * The capability token is what tells a Kindle whether sending gestures
     * is worth the bytes. It has to match the flavour actually built, so
     * this asserts against [InputSupport] rather than hardcoding either
     * answer -- it runs in both variants.
     */
    @Test
    fun readyAdvertisesNoInputOnlyInTheMirrorBuild() {
        FakeKindle().use { kindle ->
            val ready = kindle.readLine().split(" ")
            val caps = ready.drop(4)
            if (InputSupport.AVAILABLE) {
                assertTrue(
                    "control build must not claim no-input: $ready",
                    !caps.contains("no-input"),
                )
            } else {
                assertTrue(
                    "mirror build must advertise no-input: $ready",
                    caps.contains("no-input"),
                )
            }
        }
    }

    @Test
    fun refusesUnknownProtocolVersion() {
        FakeKindle(hello = "HELLO someoneelse/9 100 100").use { kindle ->
            assertTrue(kindle.readLine().startsWith("BYE unsupported-version"))
        }
    }

    @Test
    fun refusesMalformedHello() {
        FakeKindle(hello = "HELLO").use { kindle ->
            assertTrue(kindle.readLine().startsWith("BYE bad-hello"))
        }
    }

    @Test
    fun deliversFrames() {
        FakeKindle().use { kindle ->
            kindle.readLine() // READY
            server.offerFrame("first".toByteArray())
            val (seq, payload) = kindle.readFrame()
            assertEquals(1L, seq)
            assertEquals("first", String(payload))
        }
    }

    /**
     * The never-queue rule: frames produced while the Kindle has not acked
     * collapse into the newest one. 102 must never reach the wire.
     */
    @Test
    fun dropsStaleFramesWhileWaitingForAck() {
        FakeKindle().use { kindle ->
            kindle.readLine() // READY

            server.offerFrame("101".toByteArray())
            val (firstSeq, first) = kindle.readFrame()
            assertEquals("101", String(first))

            // Deliberately do not ack yet.
            server.offerFrame("102".toByteArray())
            waitFor("102 to be queued") { server.droppedFrames == 0L }
            server.offerFrame("103".toByteArray())
            waitFor("102 to be dropped") { server.droppedFrames == 1L }

            kindle.send("ACK $firstSeq")
            val (secondSeq, second) = kindle.readFrame()
            assertEquals("103", String(second))
            assertTrue("sequence must advance", secondSeq > firstSeq)
        }
    }

    /** A Kindle that never acks still gets frames, just later. */
    @Test
    fun sendsAgainAfterAckDeadline() {
        FakeKindle().use { kindle ->
            kindle.readLine() // READY
            server.offerFrame("one".toByteArray())
            kindle.readFrame()
            server.offerFrame("two".toByteArray())
            val (_, payload) = kindle.readFrame() // no ACK was sent
            assertEquals("two", String(payload))
        }
    }

    @Test
    fun parsesGestures() {
        FakeKindle().use { kindle ->
            kindle.readLine() // READY
            kindle.send("TAP 412 830")
            kindle.send("SCROLL 500")
            kindle.send("SCROLL -250")
            waitFor("gestures") { events().contains("scroll -250") }
            val seen = events()
            assertTrue(seen.contains("tap 412 830"))
            assertTrue(seen.contains("scroll 500"))
        }
    }

    @Test
    fun ignoresUnknownVerbsAndAnswersPing() {
        FakeKindle().use { kindle ->
            kindle.readLine() // READY
            kindle.send("FUTURE whatever 1 2 3")
            kindle.send("PING")
            assertEquals("PONG", kindle.readLine())
        }
    }

    @Test
    fun secondKindleReplacesTheFirst() {
        FakeKindle().use { first ->
            first.readLine() // READY
            FakeKindle().use { second ->
                assertTrue(second.readLine().startsWith("READY"))
                // The displaced session is told why it is going away.
                waitFor("first session to be replaced") {
                    events().count { it == "disconnected" } >= 1
                }
            }
        }
    }

    /** A reconnecting Kindle should not stare at a blank panel. */
    @Test
    fun resendsLastFrameToANewSession() {
        FakeKindle().use { kindle ->
            kindle.readLine()
            server.offerFrame("current-screen".toByteArray())
            kindle.readFrame()
        }
        FakeKindle().use { kindle ->
            kindle.readLine()
            val (_, payload) = kindle.readFrame()
            assertEquals("current-screen", String(payload))
        }
    }
}
