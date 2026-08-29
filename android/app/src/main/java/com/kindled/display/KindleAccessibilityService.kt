package com.kindled.display

import android.accessibilityservice.AccessibilityService
import android.accessibilityservice.GestureDescription
import android.graphics.Path
import android.util.Log
import android.view.accessibility.AccessibilityEvent

/**
 * Injects the Kindle's taps and scrolls into whatever is running on the
 * virtual display.
 *
 * An accessibility service is the only way an ordinary app can synthesise
 * touches, and since API 30 a gesture can be aimed at a specific display,
 * which is what makes the whole arrangement work: the phone's own screen is
 * not touched, only the display the Kindle is looking at.
 */
class KindleAccessibilityService : AccessibilityService() {

    companion object {
        private const val TAG = "KindleA11y"

        /** Set while the service is bound; null when the user has not enabled it. */
        @Volatile
        var instance: KindleAccessibilityService? = null
            private set

        val isEnabled: Boolean get() = instance != null

        /** Duration of an injected scroll. Long enough not to register as a fling. */
        private const val SCROLL_DURATION_MS = 250L
        private const val TAP_DURATION_MS = 60L
    }

    override fun onServiceConnected() {
        super.onServiceConnected()
        instance = this
        Log.i(TAG, "connected")
    }

    override fun onUnbind(intent: android.content.Intent?): Boolean {
        instance = null
        Log.i(TAG, "unbound")
        return super.onUnbind(intent)
    }

    override fun onDestroy() {
        instance = null
        super.onDestroy()
    }

    override fun onAccessibilityEvent(event: AccessibilityEvent?) {
        // Nothing to observe: this service exists only to dispatch gestures.
    }

    override fun onInterrupt() {}

    /** Taps [x],[y] on [displayId], in that display's coordinate space. */
    fun tap(displayId: Int, x: Float, y: Float) {
        val path = Path().apply { moveTo(x, y) }
        dispatch(displayId, path, TAP_DURATION_MS, "tap($x,$y)")
    }

    /**
     * Drags a finger from ([x],[fromY]) to ([x],[toY]) on [displayId].
     *
     * The Kindle reports scrolls in content direction, so moving the page
     * down means dragging the finger up. The caller does that conversion;
     * this method just draws the line it is given.
     */
    fun swipe(displayId: Int, x: Float, fromY: Float, toY: Float) {
        val path = Path().apply {
            moveTo(x, fromY)
            lineTo(x, toY)
        }
        dispatch(displayId, path, SCROLL_DURATION_MS, "swipe($fromY->$toY)")
    }

    private fun dispatch(displayId: Int, path: Path, durationMs: Long, what: String) {
        val stroke = GestureDescription.StrokeDescription(path, 0, durationMs)
        val builder = GestureDescription.Builder().addStroke(stroke)
        // Without this the gesture lands on the phone's own screen, which is
        // both useless and alarming.
        builder.setDisplayId(displayId)
        val ok = dispatchGesture(builder.build(), null, null)
        if (!ok) Log.w(TAG, "dispatch refused: $what on display $displayId")
    }
}
