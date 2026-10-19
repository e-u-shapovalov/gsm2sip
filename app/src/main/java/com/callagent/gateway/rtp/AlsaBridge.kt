package com.callagent.gateway.rtp

import android.util.Log
import com.callagent.gateway.AlsaBridgeConfig
import com.callagent.gateway.DeviceProfile
import com.callagent.gateway.RootShell
import java.io.DataInputStream
import java.io.InputStream
import java.io.OutputStream

/**
 * In-call audio through the root helper `mi6bridge` (raw ALSA PCM).
 *
 * The helper writes captured downlink PCM to its stdout and plays whatever
 * arrives on its stdin, both raw S16LE mono at [rate].  It keeps the playback
 * PCM fed with silence on its own, so a missing frame here costs nothing worse
 * than a gap.
 */
class AlsaBridge(private val config: AlsaBridgeConfig, val rate: Int) {

    /** Key press of the GSM party: digit, true on press, false on release. */
    @Volatile var onDtmf: ((Char, Boolean) -> Unit)? = null

    private var process: Process? = null
    private var input: DataInputStream? = null
    private var output: OutputStream? = null

    fun start(): Boolean {
        val bin = HELPER_PATHS.firstOrNull { RootShell.exec("[ -x $it ]") == 0 }
        if (bin == null) {
            Log.e(TAG, "mi6bridge not found in ${HELPER_PATHS.joinToString()}")
            return false
        }
        val setup = DeviceProfile.resolveCmd(config.setupCmd)
        if (setup.isEmpty()) {
            Log.e(TAG, "tinymix unavailable — cannot arm the ALSA routes")
            return false
        }
        val routes = RootShell.execForOutput("$setup 2>&1", timeoutMs = 8000)
        Log.i(TAG, "Routes armed: ${routes.trim().replace('\n', ' ')}")

        return try {
            val dtmf = if (config.modemDtmf) " -dtmf" else ""
            val p = ProcessBuilder(
                "su", "-c",
                "exec $bin -in ${config.captureDevice} -out ${config.playbackDevice} -rate $rate$dtmf"
            ).start()
            process = p
            input = DataInputStream(p.inputStream)
            output = p.outputStream
            Thread({ pumpLog(p.errorStream) }, "mi6bridge-log").apply { isDaemon = true }.start()
            Log.i(TAG, "mi6bridge started: capture=${config.captureDevice} playback=${config.playbackDevice} rate=$rate")
            true
        } catch (e: Exception) {
            Log.e(TAG, "mi6bridge start failed: ${e.message}")
            stop()
            false
        }
    }

    /** Blocks until [buf] is full; returns its size, or -1 once the helper is gone. */
    fun read(buf: ByteArray): Int {
        val stream = input ?: return -1
        return try {
            stream.readFully(buf)
            buf.size
        } catch (e: Exception) {
            -1
        }
    }

    fun write(buf: ByteArray) {
        try {
            output?.write(buf)
            output?.flush()
        } catch (_: Exception) {
            // The helper is gone; stop() reports and cleans up.
        }
    }

    fun stop() {
        val p = process
        process = null
        try { output?.close() } catch (_: Exception) {}
        try { input?.close() } catch (_: Exception) {}
        output = null
        input = null
        if (p != null) {
            p.destroy()
            try {
                if (!p.waitFor(2, java.util.concurrent.TimeUnit.SECONDS)) p.destroyForcibly()
            } catch (_: InterruptedException) {
                p.destroyForcibly()
            }
        }
        // su may outlive its child; make sure no helper keeps the PCMs open.
        RootShell.exec("pkill -x mi6bridge 2>/dev/null")
        val restore = DeviceProfile.resolveCmd(config.restoreCmd)
        if (restore.isNotEmpty()) RootShell.exec(restore, timeoutMs = 8000)
        Log.i(TAG, "mi6bridge stopped, routes restored")
    }

    private fun pumpLog(stream: InputStream) {
        try {
            stream.bufferedReader().forEachLine { line ->
                Log.i(TAG, "helper: $line")
                DTMF_LINE.find(line)?.let { m ->
                    onDtmf?.invoke(m.groupValues[2][0], m.groupValues[1] == "start")
                }
            }
        } catch (_: Exception) {
        }
    }

    companion object {
        private const val TAG = "AlsaBridge"
        private val HELPER_PATHS = listOf("/data/local/tmp/mi6bridge")
        private val DTMF_LINE = Regex("""DTMF (start|stop) ([0-9*#A-D])$""")
    }
}
