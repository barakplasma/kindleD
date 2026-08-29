package com.kindled.display

/**
 * Control build: the Kindle's touches are injected into the virtual display.
 *
 * This flavour declares an accessibility service, which is the only way an
 * ordinary app can synthesise touches. That declaration is also what makes
 * Play Protect block a sideloaded install, so this build has to go on over
 * ADB and needs "Allow restricted settings" before Android will let you
 * enable the service. See the README.
 */
object InputSupport {

    const val AVAILABLE = true

    val STATUS: String
        get() = if (KindleAccessibilityService.isEnabled) {
            "control build — accessibility service on"
        } else {
            "control build — accessibility service OFF, touches will not work"
        }

    fun injector(): GestureInjector? = KindleAccessibilityService.instance

    fun unavailableReason(): String =
        "Enable the Kindle Display accessibility service to send touches"
}
