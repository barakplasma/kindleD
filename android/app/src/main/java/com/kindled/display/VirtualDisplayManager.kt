package com.kindled.display

import android.app.ActivityOptions
import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.PixelFormat
import android.graphics.Rect
import android.hardware.display.DisplayManager
import android.hardware.display.VirtualDisplay
import android.media.ImageReader
import android.os.Handler
import android.os.HandlerThread
import android.util.Log

/**
 * A secondary display that exists only as a stream of images.
 *
 * An [ImageReader] surface is handed to a virtual display, an activity is
 * launched onto it, and whatever it draws arrives as frames. The physical
 * screen is not involved, which is the whole point: the Pixel's OLED can be
 * black while this keeps rendering.
 */
class VirtualDisplayManager(
    private val context: Context,
    val width: Int,
    val height: Int,
    private val densityDpi: Int,
    /** Minimum gap between captured frames. Frames arriving sooner are dropped. */
    private val minIntervalMs: Long = 333,
    /** Receives the captured bitmap and the rectangle of it that is real. */
    private val onFrame: (Bitmap, Rect) -> Unit,
) {
    companion object {
        private const val TAG = "VirtualDisplay"
        private const val DISPLAY_NAME = "kindled"
    }

    private var imageReader: ImageReader? = null
    private var virtualDisplay: VirtualDisplay? = null
    private var thread: HandlerThread? = null
    private var handler: Handler? = null

    /** Reused across frames; see FrameEncoder for why that matters. */
    private var scratch: Bitmap? = null
    private val cropRect = Rect(0, 0, width, height)
    private var lastCaptureAt = 0L

    /** Frames the capture path threw away to hold the frame rate down. */
    @Volatile
    var skippedFrames = 0L
        private set
    @Volatile
    var capturedFrames = 0L
        private set

    val displayId: Int get() = virtualDisplay?.display?.displayId ?: -1
    val isRunning: Boolean get() = virtualDisplay != null

    fun start() {
        if (virtualDisplay != null) return

        val t = HandlerThread("kindle-capture").apply { start() }
        thread = t
        val h = Handler(t.looper)
        handler = h

        // maxImages 2: one being read, one being filled. More just adds
        // latency, which is the thing this project is trying to avoid.
        val reader = ImageReader.newInstance(width, height, PixelFormat.RGBA_8888, 2)
        reader.setOnImageAvailableListener({ onImageAvailable(it) }, h)
        imageReader = reader

        val dm = context.getSystemService(Context.DISPLAY_SERVICE) as DisplayManager
        val flags = DisplayManager.VIRTUAL_DISPLAY_FLAG_PUBLIC or
            DisplayManager.VIRTUAL_DISPLAY_FLAG_PRESENTATION or
            DisplayManager.VIRTUAL_DISPLAY_FLAG_OWN_CONTENT_ONLY
        virtualDisplay = dm.createVirtualDisplay(
            DISPLAY_NAME, width, height, densityDpi, reader.surface, flags
        )
        Log.i(TAG, "virtual display ${width}x$height @${densityDpi}dpi id=$displayId")
    }

    fun stop() {
        virtualDisplay?.release()
        virtualDisplay = null
        imageReader?.close()
        imageReader = null
        thread?.quitSafely()
        thread = null
        handler = null
        scratch?.recycle()
        scratch = null
    }

    /**
     * Launches an app onto the virtual display.
     *
     * This is the step that fails on stock Android if the system decides the
     * target activity may not run on a non-default display, so the caller
     * gets a readable reason rather than a crash.
     */
    fun launch(intent: Intent): Result<Unit> {
        val display = virtualDisplay ?: return Result.failure(
            IllegalStateException("virtual display is not running")
        )
        val options = ActivityOptions.makeBasic()
            .setLaunchDisplayId(display.display.displayId)
        intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_MULTIPLE_TASK)
        return try {
            context.startActivity(intent, options.toBundle())
            Result.success(Unit)
        } catch (e: SecurityException) {
            Result.failure(e)
        } catch (e: RuntimeException) {
            Result.failure(e)
        }
    }

    /**
     * Takes the newest image, ignoring anything older. Android happily
     * produces frames far faster than e-ink can absorb them; the extras are
     * dropped here, before any encoding work is done for them.
     */
    private fun onImageAvailable(reader: ImageReader) {
        val image = try {
            reader.acquireLatestImage()
        } catch (e: IllegalStateException) {
            Log.w(TAG, "acquire failed: ${e.message}")
            null
        } ?: return

        try {
            val now = System.currentTimeMillis()
            if (now - lastCaptureAt < minIntervalMs) {
                skippedFrames++
                return
            }
            lastCaptureAt = now

            val plane = image.planes[0]
            val pixelStride = plane.pixelStride
            val rowStride = plane.rowStride
            // Rows are padded to a hardware-friendly stride, so the buffer
            // is wider than the display. Copy it whole, then crop.
            val paddedWidth = rowStride / pixelStride
            var bmp = scratch
            if (bmp == null || bmp.width != paddedWidth || bmp.height != image.height) {
                bmp?.recycle()
                bmp = Bitmap.createBitmap(paddedWidth, image.height, Bitmap.Config.ARGB_8888)
                scratch = bmp
            }
            plane.buffer.rewind()
            bmp.copyPixelsFromBuffer(plane.buffer)
            capturedFrames++
            cropRect.set(0, 0, image.width, image.height)
            onFrame(bmp, cropRect)
        } catch (e: RuntimeException) {
            Log.w(TAG, "frame capture failed: ${e.message}")
        } finally {
            image.close()
        }
    }
}
