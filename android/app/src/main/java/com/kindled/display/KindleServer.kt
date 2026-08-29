package com.kindled.display

import android.util.Log
import java.io.BufferedOutputStream
import java.io.EOFException
import java.io.IOException
import java.io.InputStream
import java.net.ServerSocket
import java.net.Socket
import java.net.SocketTimeoutException
import java.util.concurrent.atomic.AtomicBoolean

/**
 * The Pixel end of the kindled protocol: one TCP port, one Kindle, plain
 * ASCII control lines and raw JPEG payloads. See PROTOCOL.md.
 *
 * The important behaviour here is what it refuses to do. It never queues
 * frames: [offerFrame] replaces whatever was waiting, so a Kindle that falls
 * behind loses intermediate frames instead of accumulating lag.
 */
class KindleServer(
    private val port: Int = DEFAULT_PORT,
    private val frameIntervalMs: Long = 333,
    private val ackDeadlineMs: Long = 1000,
    private val listener: Listener,
) {
    /** Callbacks are delivered on the session's reader thread. */
    interface Listener {
        /**
         * Called with the Kindle's panel size before READY is sent. The
         * returned size is what will actually be streamed, letting the
         * Pixel resize its virtual display to match the panel it found.
         */
        fun onNegotiateGeometry(kindleWidth: Int, kindleHeight: Int): Pair<Int, Int>
        fun onKindleConnected(width: Int, height: Int)
        fun onKindleDisconnected(reason: String)
        fun onTap(x: Int, y: Int)
        fun onScroll(dy: Int)
    }

    companion object {
        const val DEFAULT_PORT = 45831
        private const val TAG = "KindleServer"
        private const val PROTOCOL_PREFIX = "kindled/"
        private const val MAX_LINE = 256
        private const val PING_IDLE_MS = 10_000L
        private const val READ_TIMEOUT_MS = 30_000
    }

    /** The port actually bound, which differs from [port] when it is 0. */
    @Volatile
    var boundPort: Int = 0
        private set

    /** Geometry advertised in READY; the coordinate space of every gesture. */
    @Volatile
    var frameWidth: Int = 0
    @Volatile
    var frameHeight: Int = 0

    private val running = AtomicBoolean(false)
    private var serverSocket: ServerSocket? = null
    private var acceptThread: Thread? = null

    /** Guards the single-slot frame mailbox and the session bookkeeping. */
    private val lock = Object()
    private var pendingFrame: ByteArray? = null
    private var lastFrame: ByteArray? = null
    private var nextSeq = 1L
    private var sentSeq = 0L
    private var ackedSeq = 0L
    private var sentAt = 0L
    // Written from both the reader thread (PONG) and the sender thread.
    @Volatile
    private var lastWriteAt = 0L
    private var session: Session? = null

    /** Frames dropped because a newer one arrived first; shown in the UI. */
    @Volatile
    var droppedFrames = 0L
        private set
    @Volatile
    var sentFrames = 0L
        private set

    val isConnected: Boolean get() = synchronized(lock) { session != null }

    fun start() {
        if (!running.compareAndSet(false, true)) return
        acceptThread = Thread({ acceptLoop() }, "kindle-accept").apply {
            isDaemon = true
            start()
        }
    }

    fun stop() {
        if (!running.compareAndSet(true, false)) return
        try {
            serverSocket?.close()
        } catch (e: IOException) {
            Log.w(TAG, "closing listener: ${e.message}")
        }
        val previous = synchronized(lock) {
            val old = session
            session = null
            pendingFrame = null
            lock.notifyAll()
            old
        }
        previous?.terminate("server-stopping")
        acceptThread?.join(1000)
        acceptThread = null
    }

    /**
     * Hands the newest frame to the sender, discarding any frame that has
     * not gone out yet. Cheap and non-blocking: the capture thread must
     * never wait on the network.
     */
    fun offerFrame(jpeg: ByteArray) {
        synchronized(lock) {
            if (pendingFrame != null) droppedFrames++
            pendingFrame = jpeg
            lastFrame = jpeg
            lock.notifyAll()
        }
    }

    private fun acceptLoop() {
        while (running.get()) {
            try {
                ServerSocket(port).use { server ->
                    server.reuseAddress = true
                    serverSocket = server
                    boundPort = server.localPort
                    Log.i(TAG, "listening on :$boundPort")
                    while (running.get()) {
                        val socket = server.accept()
                        handleSocket(socket)
                    }
                }
            } catch (e: IOException) {
                if (!running.get()) return
                // The hotspot going away takes the bound address with it.
                Log.w(TAG, "listener died (${e.message}); retrying")
                Thread.sleep(1000)
            }
        }
    }

    private fun handleSocket(socket: Socket) {
        socket.tcpNoDelay = true
        socket.soTimeout = READ_TIMEOUT_MS
        val next = Session(socket)
        val previous = synchronized(lock) {
            // Exactly one Kindle at a time. A reconnecting Kindle whose old
            // socket has not timed out yet must win, not be refused.
            val old = session
            session = next
            // Resend the last frame to a newcomer so the panel is not blank
            // until something on screen happens to change.
            pendingFrame = lastFrame
            sentSeq = 0
            ackedSeq = 0
            sentAt = 0
            lastWriteAt = System.currentTimeMillis()
            old
        }
        // Outside the lock: the listener is application code and must never
        // be called with the server's monitor held.
        previous?.terminate("replaced")
        next.start()
    }

    private inner class Session(private val socket: Socket) {
        private val out = BufferedOutputStream(socket.getOutputStream())
        private val alive = AtomicBoolean(true)
        private val notified = AtomicBoolean(false)
        private val writeLock = Object()
        private var readerThread: Thread? = null
        private var senderThread: Thread? = null

        fun start() {
            readerThread = Thread({ readLoop() }, "kindle-reader").apply {
                isDaemon = true
                start()
            }
        }

        /**
         * Ends the session and tells the listener exactly once, whether the
         * end came from a read error, a write error, the server stopping or
         * another Kindle taking this one's place.
         */
        fun terminate(reason: String) {
            if (alive.compareAndSet(true, false)) {
                try {
                    sendLine("BYE $reason")
                } catch (e: IOException) {
                    // The socket is already gone; that is the normal case.
                }
            }
            try {
                socket.close()
            } catch (e: IOException) {
                Log.w(TAG, "closing session: ${e.message}")
            }
            synchronized(lock) {
                // Only if this session is still the current one: a session
                // that was replaced must not unseat its successor.
                if (session === this) session = null
                lock.notifyAll()
            }
            if (notified.compareAndSet(false, true)) {
                listener.onKindleDisconnected(reason)
            }
        }

        private fun readLoop() {
            try {
                val input = socket.getInputStream()
                val hello = readLine(input) ?: throw EOFException("no HELLO")
                val parts = hello.trim().split(" ")
                if (parts.size < 4 || parts[0] != "HELLO") {
                    terminate("bad-hello")
                    return
                }
                if (!parts[1].startsWith(PROTOCOL_PREFIX)) {
                    Log.w(TAG, "refusing ${parts[1]}")
                    terminate("unsupported-version")
                    return
                }
                val kindleW = parts[2].toIntOrNull() ?: 0
                val kindleH = parts[3].toIntOrNull() ?: 0
                if (kindleW <= 0 || kindleH <= 0) {
                    terminate("bad-geometry")
                    return
                }
                val (w, h) = listener.onNegotiateGeometry(kindleW, kindleH)
                if (w <= 0 || h <= 0) {
                    terminate("no-display")
                    return
                }
                frameWidth = w
                frameHeight = h
                val fps = (1000 / frameIntervalMs).toInt()
                // Trailing capability token. Only the mirror build says
                // anything: "no-input" tells the Kindle not to send gestures
                // at a phone that will drop them. Silence means input works,
                // which is how every build behaved before capabilities
                // existed, so an older daemon is unaffected either way.
                val caps = if (InputSupport.AVAILABLE) "" else " no-input"
                sendLine("READY $frameWidth $frameHeight $fps$caps")
                Log.i(TAG, "kindle ${parts[1]} ${kindleW}x$kindleH connected")
                listener.onKindleConnected(kindleW, kindleH)

                senderThread = Thread({ sendLoop() }, "kindle-sender").apply {
                    isDaemon = true
                    start()
                }

                while (alive.get()) {
                    val line = readLine(input) ?: break
                    if (!handleLine(line)) break
                }
                terminate("closed")
            } catch (e: SocketTimeoutException) {
                terminate("read-timeout")
            } catch (e: IOException) {
                terminate(e.message ?: "io-error")
            } finally {
                synchronized(lock) { lock.notifyAll() }
            }
        }

        /** Returns false when the session should end. */
        private fun handleLine(line: String): Boolean {
            val trimmed = line.trim()
            if (trimmed.isEmpty()) return true
            val space = trimmed.indexOf(' ')
            val verb = if (space < 0) trimmed else trimmed.substring(0, space)
            val rest = if (space < 0) "" else trimmed.substring(space + 1).trim()
            when (verb) {
                "ACK" -> {
                    val seq = rest.toLongOrNull() ?: return true
                    synchronized(lock) {
                        if (seq > ackedSeq) ackedSeq = seq
                        lock.notifyAll()
                    }
                }
                "TAP" -> {
                    val f = rest.split(" ")
                    val x = f.getOrNull(0)?.toIntOrNull()
                    val y = f.getOrNull(1)?.toIntOrNull()
                    if (x != null && y != null) listener.onTap(x, y)
                }
                "SCROLL" -> rest.toIntOrNull()?.let { listener.onScroll(it) }
                "PING" -> sendLine("PONG")
                "PONG" -> Unit
                "BYE" -> return false
                // Unknown verbs are ignored on purpose: either end may grow
                // new ones without a coordinated upgrade.
                else -> Log.d(TAG, "ignoring verb $verb")
            }
            return true
        }

        /**
         * Sends at most one frame per interval, and only once the Kindle has
         * acked the previous one or the ack deadline has passed. That single
         * rule is what keeps the link latent-free: everything produced while
         * the Kindle is painting collapses into one newest frame.
         */
        private fun sendLoop() {
            try {
                while (alive.get()) {
                    val frame: ByteArray
                    val seq: Long
                    synchronized(lock) {
                        while (alive.get()) {
                            val now = System.currentTimeMillis()
                            val waited = now - sentAt
                            val acked = ackedSeq >= sentSeq
                            val ready = pendingFrame != null &&
                                waited >= frameIntervalMs &&
                                (acked || waited >= ackDeadlineMs)
                            if (ready) break
                            val delay = when {
                                pendingFrame == null -> PING_IDLE_MS
                                !acked -> ackDeadlineMs - waited
                                else -> frameIntervalMs - waited
                            }
                            lock.wait(delay.coerceIn(1, PING_IDLE_MS))
                            // Keepalive is tracked separately from frame
                            // pacing, so a PING never delays a frame.
                            if (System.currentTimeMillis() - lastWriteAt >= PING_IDLE_MS) {
                                sendLine("PING")
                            }
                        }
                        if (!alive.get()) return
                        frame = pendingFrame!!
                        pendingFrame = null
                        seq = nextSeq++
                        sentSeq = seq
                        sentAt = System.currentTimeMillis()
                    }
                    sendFrame(seq, frame)
                    sentFrames++
                }
            } catch (e: IOException) {
                terminate(e.message ?: "write-error")
            } catch (e: InterruptedException) {
                Thread.currentThread().interrupt()
            }
        }

        private fun sendFrame(seq: Long, jpeg: ByteArray) {
            synchronized(writeLock) {
                out.write("FRAME $seq ${jpeg.size}\n".toByteArray(Charsets.US_ASCII))
                out.write(jpeg)
                out.flush()
            }
            lastWriteAt = System.currentTimeMillis()
        }

        private fun sendLine(line: String) {
            synchronized(writeLock) {
                out.write("$line\n".toByteArray(Charsets.US_ASCII))
                out.flush()
            }
            lastWriteAt = System.currentTimeMillis()
        }
    }

    /**
     * Reads one control line, refusing anything longer than the protocol
     * allows so a desynchronised stream fails fast instead of being eaten.
     */
    private fun readLine(input: InputStream): String? {
        val buf = StringBuilder()
        while (true) {
            val b = input.read()
            if (b < 0) return if (buf.isEmpty()) null else buf.toString()
            if (b == '\n'.code) return buf.toString()
            if (b == '\r'.code) continue
            buf.append(b.toChar())
            if (buf.length > MAX_LINE) throw IOException("control line too long")
        }
    }
}
