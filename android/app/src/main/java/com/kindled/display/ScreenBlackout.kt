package com.kindled.display

import android.content.Context
import android.graphics.Color
import android.graphics.PixelFormat
import android.os.Build
import android.provider.Settings
import android.util.Log
import android.view.Gravity
import android.view.View
import android.view.WindowManager

/**
 * Blacks out the phone while the Kindle is in use.
 *
 * The obvious approach — let the phone sleep — does not work: a virtual
 * display without the system's trusted-display privilege lives in the
 * default display group, and that whole group sleeps when the physical
 * screen does, taking the Kindle's frames with it.
 *
 * So instead of turning the screen off, this makes it cost nothing: a black
 * overlay at minimum brightness. On the Pixel's OLED, black pixels are
 * unlit, so the panel draws roughly what an off panel draws, while the
 * display group stays awake and the virtual display keeps rendering.
 */
class ScreenBlackout(private val context: Context) {

    companion object {
        private const val TAG = "ScreenBlackout"
    }

    private var windowManager: WindowManager? = null
    private var view: View? = null

    val isPermitted: Boolean get() = Settings.canDrawOverlays(context)
    val isShowing: Boolean get() = view != null

    fun show(): Boolean {
        if (view != null) return true
        if (!isPermitted) {
            Log.w(TAG, "no overlay permission")
            return false
        }
        val wm = context.getSystemService(Context.WINDOW_SERVICE) as WindowManager
        val v = View(context).apply { setBackgroundColor(Color.BLACK) }
        val params = WindowManager.LayoutParams(
            WindowManager.LayoutParams.MATCH_PARENT,
            WindowManager.LayoutParams.MATCH_PARENT,
            WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY,
            WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON or
                WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE or
                WindowManager.LayoutParams.FLAG_LAYOUT_IN_SCREEN,
            PixelFormat.OPAQUE,
        ).apply {
            gravity = Gravity.TOP or Gravity.START
            // Not zero: some devices treat 0 as "use the system value".
            screenBrightness = 0.01f
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                fitInsetsTypes = 0
            }
        }
        return try {
            wm.addView(v, params)
            windowManager = wm
            view = v
            Log.i(TAG, "screen blacked out")
            true
        } catch (e: WindowManager.BadTokenException) {
            Log.w(TAG, "overlay refused: ${e.message}")
            false
        }
    }

    fun hide() {
        val v = view ?: return
        try {
            windowManager?.removeView(v)
        } catch (e: IllegalArgumentException) {
            Log.w(TAG, "overlay already gone")
        }
        view = null
    }
}
