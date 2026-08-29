package com.kindled.display

import android.Manifest
import android.app.Activity
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Color
import android.graphics.Typeface
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.widget.ArrayAdapter
import android.widget.Button
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.Spinner
import android.widget.TextView

/**
 * The whole user interface: pick an app, start the link, black out the
 * screen. The UI is built in code because a layout file, a theme and a
 * resource pipeline would be more machinery than this screen deserves.
 */
class MainActivity : Activity() {

    private lateinit var statusView: TextView
    private lateinit var networkView: TextView
    private lateinit var permissionView: TextView
    private lateinit var appSpinner: Spinner
    private lateinit var startButton: Button
    private lateinit var blackoutButton: Button

    private val handler = Handler(Looper.getMainLooper())
    private var apps: List<AppEntry> = emptyList()
    private var blackedOut = false

    private data class AppEntry(val label: String, val packageName: String) {
        override fun toString() = label
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(buildUi())
        loadApps()
        requestNotificationPermissionIfNeeded()
    }

    override fun onResume() {
        super.onResume()
        KindleDisplayService.statusListener = { text ->
            handler.post { statusView.text = text }
        }
        refresh()
    }

    override fun onPause() {
        KindleDisplayService.statusListener = null
        super.onPause()
    }

    private fun buildUi(): View {
        val pad = (16 * resources.displayMetrics.density).toInt()
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad, pad, pad)
        }

        root.addView(TextView(this).apply {
            text = "Kindle Display"
            textSize = 24f
            setTypeface(typeface, Typeface.BOLD)
        })

        statusView = TextView(this).apply {
            text = KindleDisplayService.status
            textSize = 16f
            setPadding(0, pad, 0, pad / 2)
        }
        root.addView(statusView)

        networkView = TextView(this).apply {
            textSize = 14f
            setTextColor(Color.DKGRAY)
            setPadding(0, 0, 0, pad)
        }
        root.addView(networkView)

        root.addView(label("App to show on the Kindle"))
        appSpinner = Spinner(this)
        root.addView(appSpinner)

        startButton = button("Start") { toggleService() }
        root.addView(startButton)

        blackoutButton = button("Black out phone screen") { toggleBlackout() }
        root.addView(blackoutButton)

        root.addView(label("Permissions"))
        permissionView = TextView(this).apply {
            textSize = 14f
            setTextColor(Color.DKGRAY)
        }
        root.addView(permissionView)

        root.addView(button("Accessibility settings") {
            startActivity(Intent(Settings.ACTION_ACCESSIBILITY_SETTINGS))
        })
        root.addView(button("Overlay permission") {
            startActivity(
                Intent(
                    Settings.ACTION_MANAGE_OVERLAY_PERMISSION,
                    Uri.parse("package:$packageName"),
                )
            )
        })

        root.addView(TextView(this).apply {
            text = HELP
            textSize = 13f
            setTextColor(Color.DKGRAY)
            setPadding(0, pad, 0, 0)
        })

        return ScrollView(this).apply {
            addView(
                root,
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT,
            )
        }
    }

    private fun label(text: String) = TextView(this).apply {
        this.text = text
        textSize = 14f
        setTypeface(typeface, Typeface.BOLD)
        setPadding(0, (12 * resources.displayMetrics.density).toInt(), 0, 0)
    }

    private fun button(text: String, onClick: () -> Unit) = Button(this).apply {
        this.text = text
        gravity = Gravity.CENTER
        setOnClickListener { onClick() }
    }

    /** Lists apps that actually have a launcher activity to put on screen. */
    private fun loadApps() {
        val intent = Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER)
        val resolved = packageManager.queryIntentActivities(intent, 0)
        apps = resolved
            .map {
                AppEntry(
                    it.loadLabel(packageManager).toString(),
                    it.activityInfo.packageName,
                )
            }
            .distinctBy { it.packageName }
            .filter { it.packageName != packageName }
            .sortedBy { it.label.lowercase() }

        appSpinner.adapter = ArrayAdapter(
            this,
            android.R.layout.simple_spinner_dropdown_item,
            apps,
        )
        // Chrome is the app this was built for, so preselect it if present.
        val chrome = apps.indexOfFirst { it.packageName.contains("chrome") }
        if (chrome >= 0) appSpinner.setSelection(chrome)
    }

    private fun toggleService() {
        if (KindleDisplayService.isRunning) {
            startService(
                Intent(this, KindleDisplayService::class.java)
                    .setAction(KindleDisplayService.ACTION_STOP)
            )
        } else {
            val selected = appSpinner.selectedItem as? AppEntry
            val intent = Intent(this, KindleDisplayService::class.java)
                .setAction(KindleDisplayService.ACTION_START)
                .putExtra(KindleDisplayService.EXTRA_PACKAGE, selected?.packageName)
            startForegroundService(intent)
        }
        handler.postDelayed({ refresh() }, 400)
    }

    private fun toggleBlackout() {
        if (!Settings.canDrawOverlays(this)) {
            statusView.text = "Grant the overlay permission first"
            return
        }
        blackedOut = !blackedOut
        startService(
            Intent(this, KindleDisplayService::class.java)
                .setAction(KindleDisplayService.ACTION_BLACKOUT)
                .putExtra(KindleDisplayService.EXTRA_BLACKOUT, blackedOut)
        )
        refresh()
    }

    private fun refresh() {
        statusView.text = KindleDisplayService.status
        networkView.text = HotspotNetwork.describe(KindleServer.DEFAULT_PORT)
        startButton.text = if (KindleDisplayService.isRunning) "Stop" else "Start"
        blackoutButton.text =
            if (blackedOut) "Restore phone screen" else "Black out phone screen"

        val overlay = if (Settings.canDrawOverlays(this)) "on" else "OFF — cannot black out screen"
        permissionView.text = "Input: ${InputSupport.STATUS}\nOverlay: $overlay"
    }

    private fun requestNotificationPermissionIfNeeded() {
        // The permission only exists from API 33; asking for it on 30-32
        // does nothing useful and the constant would be inlined blindly.
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
            != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
    }

    private companion object {
        val HELP = """
            Travel setup:
              1. Turn on the Pixel hotspot.
              2. The Kindle joins it and kindled reconnects on its own.
              3. Pick an app above and press Start.
              4. Black out the phone screen and read on the Kindle.

            The Kindle dials this phone on port ${KindleServer.DEFAULT_PORT}
            at the hotspot gateway address, so it does not matter what
            address the Kindle itself was given.
        """.trimIndent()
    }
}
