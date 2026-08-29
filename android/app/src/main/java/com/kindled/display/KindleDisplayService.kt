package com.kindled.display

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.graphics.Bitmap
import android.graphics.Rect
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.util.DisplayMetrics
import android.util.Log
import android.view.WindowManager
import kotlin.math.abs
import kotlin.math.min

/**
 * The long-lived half of the Pixel side: owns the virtual display, the
 * capture and encode path, the TCP server, and the wakelock that keeps all
 * of it running while the phone looks asleep.
 *
 * It is a foreground service because everything here is exactly what
 * Android kills first otherwise.
 */
class KindleDisplayService : Service(), KindleServer.Listener {

    companion object {
        private const val TAG = "KindleDisplay"
        private const val CHANNEL_ID = "kindled"
        private const val NOTIFICATION_ID = 1

        const val ACTION_START = "com.kindled.display.START"
        const val ACTION_STOP = "com.kindled.display.STOP"
        const val ACTION_BLACKOUT = "com.kindled.display.BLACKOUT"
        const val EXTRA_PACKAGE = "package"
        const val EXTRA_BLACKOUT = "blackout"

        /** Default panel size: 2024 Paperwhite class. Renegotiated on connect. */
        const val DEFAULT_WIDTH = 1072
        const val DEFAULT_HEIGHT = 1448

        @Volatile
        var isRunning = false
            private set

        /** Set by the UI while it is visible. */
        @Volatile
        var statusListener: ((String) -> Unit)? = null

        @Volatile
        var status: String = "Stopped"
            private set

        private fun publish(text: String) {
            status = text
            statusListener?.invoke(text)
        }
    }

    private var server: KindleServer? = null
    private var display: VirtualDisplayManager? = null
    private var encoder: FrameEncoder? = null
    private var blackout: ScreenBlackout? = null
    private var wakeLock: PowerManager.WakeLock? = null

    /** Package to launch onto the virtual display, if any. */
    private var targetPackage: String? = null

    /** Serialises display teardown/rebuild against the capture callback. */
    private val displayLock = Object()

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopEverything()
                stopSelf()
                return START_NOT_STICKY
            }
            ACTION_BLACKOUT -> {
                val on = intent.getBooleanExtra(EXTRA_BLACKOUT, false)
                if (on) blackout?.show() else blackout?.hide()
                updateNotification()
                return START_STICKY
            }
            else -> {
                intent?.getStringExtra(EXTRA_PACKAGE)?.let { targetPackage = it }
                startEverything()
                return START_STICKY
            }
        }
    }

    override fun onDestroy() {
        stopEverything()
        super.onDestroy()
    }

    private fun startEverything() {
        if (isRunning) {
            launchTarget()
            return
        }
        createChannel()
        startForegroundTyped()

        val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
        // Partial only: this keeps the CPU alive, not the screen. Held
        // without a timeout on purpose -- the link is meant to survive a
        // whole flight -- and released in stopEverything.
        @Suppress("WakelockTimeout")
        wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "kindled:link").apply {
            setReferenceCounted(false)
            acquire()
        }

        blackout = ScreenBlackout(this)
        buildDisplay(DEFAULT_WIDTH, DEFAULT_HEIGHT)

        val srv = KindleServer(listener = this)
        srv.frameWidth = DEFAULT_WIDTH
        srv.frameHeight = DEFAULT_HEIGHT
        srv.start()
        server = srv

        isRunning = true
        publish(HotspotNetwork.describe(KindleServer.DEFAULT_PORT))
        updateNotification()
        launchTarget()
    }

    private fun stopEverything() {
        isRunning = false
        server?.stop()
        server = null
        synchronized(displayLock) {
            display?.stop()
            display = null
            encoder?.release()
            encoder = null
        }
        blackout?.hide()
        blackout = null
        wakeLock?.let { if (it.isHeld) it.release() }
        wakeLock = null
        publish("Stopped")
        stopForeground(STOP_FOREGROUND_REMOVE)
    }

    /** Builds (or rebuilds) the virtual display and the encoder for it. */
    private fun buildDisplay(width: Int, height: Int) {
        synchronized(displayLock) {
            display?.stop()
            encoder?.release()

            encoder = FrameEncoder(width, height)
            val vd = VirtualDisplayManager(
                context = this,
                width = width,
                height = height,
                densityDpi = densityFor(width, height),
                onFrame = ::onFrameCaptured,
            )
            vd.start()
            display = vd
        }
    }

    /**
     * Picks a density for the virtual display.
     *
     * This is the single most consequential number for readability: it
     * decides how large text and buttons are on the Kindle. A Kindle-sized
     * panel at phone density renders a comically zoomed page, so the density
     * is scaled to keep roughly a phone's worth of content on screen.
     */
    private fun densityFor(width: Int, height: Int): Int {
        val wm = getSystemService(Context.WINDOW_SERVICE) as WindowManager
        val metrics = DisplayMetrics().also {
            @Suppress("DEPRECATION")
            wm.defaultDisplay.getRealMetrics(it)
        }
        val phoneWidth = if (metrics.widthPixels > 0) metrics.widthPixels else 1080
        val phoneDensity = if (metrics.densityDpi > 0) metrics.densityDpi else 420
        // Keep the same pixels-per-dp relationship the phone has, scaled to
        // the panel's width, then bias slightly denser: e-ink is read closer.
        val scaled = phoneDensity.toFloat() * width / phoneWidth
        return (scaled * 0.9f).toInt().coerceIn(120, 480)
    }

    /** Called on the capture thread for every frame we decided to keep. */
    private fun onFrameCaptured(bitmap: Bitmap, crop: Rect) {
        val jpeg = synchronized(displayLock) {
            val enc = encoder ?: return
            try {
                enc.encode(bitmap, crop)
            } catch (e: RuntimeException) {
                Log.w(TAG, "encode failed: ${e.message}")
                return
            }
        }
        server?.offerFrame(jpeg)
    }

    private fun launchTarget() {
        val pkg = targetPackage ?: return
        val vd = display ?: return
        val intent = packageManager.getLaunchIntentForPackage(pkg)
        if (intent == null) {
            publish("Cannot launch $pkg: no launcher activity")
            return
        }
        vd.launch(intent).onFailure { e ->
            // The usual cause is the system refusing to put another app's
            // activity on a display this app owns. Say so plainly.
            publish("Launch refused for $pkg: ${e.message}")
            Log.w(TAG, "launch failed", e)
        }.onSuccess {
            publish("Streaming $pkg on display ${vd.displayId}")
        }
        updateNotification()
    }

    // ---- KindleServer.Listener ------------------------------------------

    override fun onNegotiateGeometry(kindleWidth: Int, kindleHeight: Int): Pair<Int, Int> {
        val current = display
        if (current == null) {
            buildDisplay(kindleWidth, kindleHeight)
            launchTarget()
        } else if (current.width != kindleWidth || current.height != kindleHeight) {
            // A different Kindle, or a rotated one: match the panel we found
            // rather than making the Kindle rescale anything.
            Log.i(TAG, "resizing display to ${kindleWidth}x$kindleHeight")
            buildDisplay(kindleWidth, kindleHeight)
            launchTarget()
        }
        val vd = display ?: return Pair(0, 0)
        return Pair(vd.width, vd.height)
    }

    override fun onKindleConnected(width: Int, height: Int) {
        publish("Kindle connected (${width}x$height)")
        updateNotification()
    }

    override fun onKindleDisconnected(reason: String) {
        publish("Kindle disconnected: $reason")
        updateNotification()
    }

    override fun onTap(x: Int, y: Int) {
        val vd = display ?: return
        val a11y = KindleAccessibilityService.instance ?: run {
            publish("Enable the accessibility service to send taps")
            return
        }
        a11y.tap(
            vd.displayId,
            x.toFloat().coerceIn(0f, (vd.width - 1).toFloat()),
            y.toFloat().coerceIn(0f, (vd.height - 1).toFloat()),
        )
    }

    /**
     * Turns a content-direction scroll into a swipe.
     *
     * SCROLL 500 means "move the page 500px further down", which on a
     * touchscreen is a finger travelling 500px *up*. The swipe is centred on
     * the display and clamped to leave margins, so it never starts or ends
     * in the gesture-navigation strips at the edges.
     */
    override fun onScroll(dy: Int) {
        val vd = display ?: return
        val a11y = KindleAccessibilityService.instance ?: run {
            publish("Enable the accessibility service to send scrolls")
            return
        }
        if (dy == 0) return
        val margin = vd.height * 0.1f
        val usable = vd.height - 2 * margin
        val span = min(abs(dy).toFloat(), usable)
        val centerY = vd.height / 2f
        val direction = if (dy > 0) 1f else -1f
        val fromY = centerY + direction * span / 2f
        val toY = centerY - direction * span / 2f
        a11y.swipe(vd.displayId, vd.width / 2f, fromY, toY)
    }

    // ---- Notification ----------------------------------------------------

    private fun createChannel() {
        val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        val channel = NotificationChannel(
            CHANNEL_ID,
            "Kindle display link",
            NotificationManager.IMPORTANCE_LOW,
        ).apply { setShowBadge(false) }
        nm.createNotificationChannel(channel)
    }

    private fun startForegroundTyped() {
        // minSdk is 30, so the typed overload always exists here.
        startForeground(
            NOTIFICATION_ID,
            buildNotification(),
            ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE,
        )
    }

    private fun buildNotification(): Notification {
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE,
        )
        val stop = PendingIntent.getService(
            this, 1, Intent(this, KindleDisplayService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE,
        )
        return Notification.Builder(this, CHANNEL_ID)
            .setContentTitle("Kindle display")
            .setContentText(status)
            .setSmallIcon(android.R.drawable.stat_sys_data_bluetooth)
            .setContentIntent(open)
            .addAction(
                Notification.Action.Builder(
                    android.graphics.drawable.Icon.createWithResource(
                        this, android.R.drawable.ic_menu_close_clear_cancel
                    ),
                    "Stop",
                    stop,
                ).build()
            )
            .setOngoing(true)
            .build()
    }

    private fun updateNotification() {
        if (!isRunning) return
        val nm = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        nm.notify(NOTIFICATION_ID, buildNotification())
    }
}
