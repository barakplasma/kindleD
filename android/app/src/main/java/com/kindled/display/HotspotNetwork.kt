package com.kindled.display

import java.net.Inet4Address
import java.net.NetworkInterface

/**
 * Works out which address the Kindle should be dialling.
 *
 * The Kindle finds the Pixel by asking for its default gateway, so this is
 * only ever used to show a human what to expect, and to give a useful error
 * when the hotspot is off and there is nothing to connect to.
 */
object HotspotNetwork {

    /** Interface name prefixes Android uses for tethering. */
    private val TETHER_PREFIXES = listOf("ap", "wlan1", "swlan", "softap", "rndis", "usb")

    data class Address(val iface: String, val ip: String, val isHotspot: Boolean)

    /** Every usable IPv4 address on the device, hotspot interfaces first. */
    fun localAddresses(): List<Address> {
        val out = mutableListOf<Address>()
        val interfaces = try {
            NetworkInterface.getNetworkInterfaces() ?: return emptyList()
        } catch (e: java.net.SocketException) {
            return emptyList()
        }
        for (iface in interfaces) {
            if (!iface.isUp || iface.isLoopback) continue
            val hotspot = TETHER_PREFIXES.any { iface.name.startsWith(it) }
            for (addr in iface.inetAddresses) {
                if (addr !is Inet4Address || addr.isLoopbackAddress) continue
                out.add(Address(iface.name, addr.hostAddress ?: continue, hotspot))
            }
        }
        return out.sortedByDescending { it.isHotspot }
    }

    /** The address the Kindle will most likely reach, or null if offline. */
    fun bestAddress(): Address? = localAddresses().firstOrNull()

    /** A one-line summary for the status display. */
    fun describe(port: Int): String {
        val addrs = localAddresses()
        if (addrs.isEmpty()) return "No network. Turn on the Pixel hotspot."
        val best = addrs.first()
        val label = if (best.isHotspot) "hotspot" else best.iface
        return "Listening on ${best.ip}:$port ($label)"
    }
}
