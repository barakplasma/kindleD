package com.kindled.display

/**
 * Somewhere to send the Kindle's taps and scrolls.
 *
 * The mirror build has no implementation of this at all: injecting touches
 * requires an accessibility service, and merely declaring one makes Play
 * Protect refuse to install a sideloaded app. Keeping the injection behind
 * an interface is what lets the accessibility service live entirely in the
 * control flavour, so the mirror build's manifest never mentions it.
 */
interface GestureInjector {

    /** Taps [x],[y] on [displayId], in that display's coordinate space. */
    fun tap(displayId: Int, x: Float, y: Float)

    /** Drags a finger from ([x],[fromY]) to ([x],[toY]) on [displayId]. */
    fun swipe(displayId: Int, x: Float, fromY: Float, toY: Float)
}
