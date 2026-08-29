package com.kindled.display

import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.ColorMatrix
import android.graphics.ColorMatrixColorFilter
import android.graphics.Paint
import android.graphics.Rect
import java.io.ByteArrayOutputStream

/**
 * Turns a captured RGBA frame into the grayscale JPEG the Kindle wants.
 *
 * Everything reusable is reused. At 3 FPS the CPU cost is irrelevant, but
 * the allocation churn is not: a fresh full-size bitmap three times a second
 * is a steady stream of large objects for the GC to move around while the
 * phone is meant to be sitting in a bag doing nothing.
 */
class FrameEncoder(
    private val outWidth: Int,
    private val outHeight: Int,
    private val quality: Int = 70,
    contrast: Float = 1.15f,
) {
    private val paint = Paint(Paint.FILTER_BITMAP_FLAG or Paint.ANTI_ALIAS_FLAG).apply {
        colorFilter = ColorMatrixColorFilter(grayscaleMatrix(contrast))
    }
    private val scratch = Bitmap.createBitmap(outWidth, outHeight, Bitmap.Config.ARGB_8888)
    private val canvas = Canvas(scratch)
    private val dst = Rect(0, 0, outWidth, outHeight)
    private val buffer = ByteArrayOutputStream(256 * 1024)

    /**
     * Scales [src] out of [source] to the panel size, drains the colour out
     * of it and encodes it. [src] matters because a captured frame is wider
     * than the display: rows are padded to the hardware stride. The returned
     * array is freshly allocated because it is handed to the network thread.
     */
    fun encode(source: Bitmap, src: Rect? = null): ByteArray {
        canvas.drawColor(android.graphics.Color.WHITE)
        canvas.drawBitmap(source, src, dst, paint)
        buffer.reset()
        scratch.compress(Bitmap.CompressFormat.JPEG, quality, buffer)
        return buffer.toByteArray()
    }

    fun release() {
        scratch.recycle()
    }

    private companion object {
        /**
         * Saturation zero, then a little contrast around mid grey. E-ink
         * renders a flat grey ramp as mush; nudging the midtones apart
         * costs nothing and makes text noticeably crisper.
         */
        fun grayscaleMatrix(contrast: Float): ColorMatrix {
            val gray = ColorMatrix().apply { setSaturation(0f) }
            val shift = (1f - contrast) * 128f
            val stretch = ColorMatrix(
                floatArrayOf(
                    contrast, 0f, 0f, 0f, shift,
                    0f, contrast, 0f, 0f, shift,
                    0f, 0f, contrast, 0f, shift,
                    0f, 0f, 0f, 1f, 0f,
                )
            )
            stretch.preConcat(gray)
            return stretch
        }
    }
}
