package com.kindled.display

/**
 * Mirror build: the Kindle is a screen, not a controller.
 *
 * There is no accessibility service in this flavour and no reference to one,
 * so the manifest carries no BIND_ACCESSIBILITY_SERVICE and the APK installs
 * without Play Protect's sideloading block or the restricted-settings gate.
 * Install the control build if you want the Kindle's touches to do something.
 */
object InputSupport {

    /** Whether this build can act on the Kindle's gestures at all. */
    const val AVAILABLE = false

    /** Shown in the app's status line. */
    const val STATUS = "mirror build — Kindle touches are ignored"

    /** Always null here; the control flavour returns the bound service. */
    fun injector(): GestureInjector? = null

    /** Why [injector] returned null, for the status line and the log. */
    fun unavailableReason(): String = STATUS
}
