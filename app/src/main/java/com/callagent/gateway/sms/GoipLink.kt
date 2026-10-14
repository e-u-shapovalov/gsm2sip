package com.callagent.gateway.sms

import android.content.Context
import android.os.Handler
import android.os.HandlerThread
import android.telephony.TelephonyManager
import android.util.Log
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.SocketTimeoutException
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

/**
 * This phone as one line of a GoIP SMS server (the "SMS Server" protocol GoIP
 * gateways speak to goip-bridge and similar), so SMS and USSD go through the
 * same queue and webhooks as the hardware GoIP lines.
 *
 * Device side of the protocol, UDP, text, one message per datagram:
 *   keepalive  -> req:<n>;id:<line>;pass:<p>;num:..;signal:..;gsm_status:LOGIN;...
 *              <- reg:<n>;status:200;...
 *   inbound    -> RECEIVE:<n>;id:<line>;password:<p>;srcnum:<from>;msg:<text>
 *              <- RECEIVE <n> OK                    (repeat until acked)
 *   send       <- MSG <id> <len> <text>             -> PASSWORD <id>
 *              <- PASSWORD <id> <p>                  -> SEND <id>
 *              <- SEND <id> <ref> <number>           -> WAIT <id> <ref>, then
 *                                                       OK <id> <ref> <sms_no> | ERROR <id> <ref> errorstatus:<n>
 *              <- DONE <id>
 *   report     -> DELIVER:<n>;id:<line>;password:<p>;sms_no:<k>;state:<0|tp-status>
 *              <- DELIVER <n> OK
 *   ussd       <- USSD <id> <p> <code>               -> USSD <id> <reply> | USSDERROR <id> <why>
 */
class GoipLink(
    private val context: Context,
    private val host: String,
    private val port: Int,
    private val line: String,
    private val password: String,
    private val ownNumber: String,
    /** Source address; empty = the default network.  Set when the server is
     *  reached through a tunnel on another interface (same as SIP local_ip). */
    private val localIp: String,
    private val listener: Listener,
) {
    interface Listener {
        /** The server asks to send an SMS.  Return false if it cannot even be queued. */
        fun onSendRequest(session: String, ref: String, number: String, text: String, smsNo: Int): Boolean
        fun onLog(line: String)
    }

    @Volatile var registered = false; private set
    @Volatile private var running = false
    private var socket: DatagramSocket? = null
    private var server: InetAddress? = null
    private val seq = AtomicInteger((System.currentTimeMillis() / 1000 % 100000).toInt())
    private val smsNo = AtomicInteger(0)
    private val acks = ConcurrentHashMap<String, CountDownLatch>()
    private val sessions = ConcurrentHashMap<String, Session>()
    private var ussdThread: HandlerThread? = null

    private class Session(val id: String) {
        var text: String = ""
        var submitted = false
        @Volatile var lastSeen = System.currentTimeMillis()
    }

    fun start() {
        if (running) return
        running = true
        Thread({ runLoop() }, "goip-link").start()
    }

    fun stop() {
        running = false
        registered = false
        try { socket?.close() } catch (_: Exception) {}
        ussdThread?.quitSafely()
        ussdThread = null
    }

    private fun runLoop() {
        try {
            server = InetAddress.getByName(host)
            val s = (if (localIp.isNotEmpty()) DatagramSocket(java.net.InetSocketAddress(localIp, 0))
                     else DatagramSocket()).apply { soTimeout = 1000 }
            socket = s
            listener.onLog("GoIP: line $line -> $host:$port from ${s.localSocketAddress}")
            Thread({ receiveLoop(s) }, "goip-recv").start()
            while (running) {
                sendKeepalive()
                sleepQuiet(KEEPALIVE_MS)
                sessions.values.removeAll { System.currentTimeMillis() - it.lastSeen > SESSION_TTL_MS }
            }
        } catch (e: Exception) {
            if (running) Log.e(TAG, "GoIP link failed: ${e.message}", e)
            listener.onLog("GoIP: link failed — ${e.message}")
        }
    }

    private fun receiveLoop(s: DatagramSocket) {
        val buf = ByteArray(4096)
        while (running) {
            try {
                val p = DatagramPacket(buf, buf.size)
                s.receive(p)
                handle(String(p.data, 0, p.length, Charsets.UTF_8).trimEnd('\r', '\n'))
            } catch (_: SocketTimeoutException) {
            } catch (e: Exception) {
                if (running) Log.w(TAG, "GoIP receive: ${e.message}")
            }
        }
    }

    private fun send(msg: String) {
        val s = socket ?: return
        val addr = server ?: return
        try {
            val b = msg.toByteArray(Charsets.UTF_8)
            s.send(DatagramPacket(b, b.size, addr, port))
        } catch (e: Exception) {
            Log.w(TAG, "GoIP send failed: ${e.message}")
        }
    }

    // ── keepalive ────────────────────────────────────────

    private fun sendKeepalive() {
        val tm = context.getSystemService(TelephonyManager::class.java)
        val carrier = clean(tm?.networkOperatorName.orEmpty()).ifEmpty { "GSM" }
        // GoIP reports the modem's CSQ, 0..31.  Android gives 0..4 bars.
        val signal = (tm?.signalStrength?.level ?: 0) * 31 / 4
        val num = ownNumber.filter { it.isDigit() }
        send(
            "req:${seq.incrementAndGet()};id:$line;pass:$password;num:$num;signal:$signal;" +
                "gsm_status:LOGIN;voip_status:LOGIN;voip_state:IDLE;remain_time:-1;" +
                "imei:;imsi:;iccid:;pro:$carrier;"
        )
    }

    // ── inbound from the server ──────────────────────────

    private fun handle(p: String) {
        if (p.startsWith("reg:")) {
            val ok = p.contains("status:200")
            if (ok != registered) listener.onLog("GoIP: line $line ${if (ok) "registered" else "rejected: $p"}")
            registered = ok
            return
        }
        // Never the password: PASSWORD and USSD packets carry it.
        Log.d(TAG, "GoIP <- ${p.replace(password, "***").take(160)}")
        val tok = p.split(' ')
        if (tok.size < 2) {
            Log.w(TAG, "GoIP: unparsed packet: ${p.replace(password, "***").take(160)}")
            return
        }
        val cmd = tok[0]
        val id = tok[1]
        when (cmd) {
            // Acks for what we sent: "RECEIVE <n> OK", "DELIVER <n> OK".
            "RECEIVE", "DELIVER" -> if (tok.getOrNull(2) == "OK") acks.remove("$cmd:$id")?.countDown()
            "MSG" -> {
                // MSG <id> <len> <text...> — the text may hold spaces.
                val text = p.split(' ', limit = 4).getOrNull(3).orEmpty()
                sessions.getOrPut(id) { Session(id) }.apply { this.text = text; lastSeen = System.currentTimeMillis() }
                send("PASSWORD $id")
            }
            "PASSWORD" -> {
                val sess = sessions[id] ?: return
                sess.lastSeen = System.currentTimeMillis()
                if (tok.getOrNull(2) == password) send("SEND $id") else send("ERROR $id PASSWORD")
            }
            "SEND" -> {
                val sess = sessions[id] ?: return
                val ref = tok.getOrNull(2) ?: return
                val number = tok.getOrNull(3) ?: return
                sess.lastSeen = System.currentTimeMillis()
                send("WAIT $id $ref")
                if (sess.submitted) return       // a repeated SEND must not send twice
                sess.submitted = true
                val no = nextSmsNo()
                listener.onLog("GoIP: send to $number (${sess.text.length} chars) session $id")
                if (!listener.onSendRequest(id, ref, number, sess.text, no)) {
                    send("ERROR $id $ref errorstatus:500")
                }
            }
            "DONE" -> sessions.remove(id)
            "USSD" -> {
                // USSD <id> <pass> <code>
                if (tok.getOrNull(2) != password) {
                    send("USSDERROR $id PASSWORD")
                    return
                }
                runUssd(id, tok.getOrNull(3).orEmpty())
            }
        }
    }

    // ── results back to the server ───────────────────────

    /** The modem took the message (or refused it). */
    fun reportSent(session: String, ref: String, ok: Boolean, smsNo: Int, error: Int) {
        if (ok) send("OK $session $ref $smsNo") else send("ERROR $session $ref errorstatus:$error")
    }

    /** Delivery report; state 0 = delivered, otherwise the TP-Status. */
    fun reportDelivered(smsNo: Int, state: Int): Boolean {
        val n = seq.incrementAndGet().toString()
        return sendAcked(
            "DELIVER", n,
            "DELIVER:$n;id:$line;password:$password;sms_no:$smsNo;state:$state"
        )
    }

    /** Hand an inbound SMS to the server; true once it acknowledged. */
    fun deliverInbound(from: String, text: String): Boolean {
        if (!registered) return false
        val n = seq.incrementAndGet().toString()
        return sendAcked(
            "RECEIVE", n,
            "RECEIVE:$n;id:$line;password:$password;srcnum:$from;msg:$text"
        )
    }

    private fun sendAcked(kind: String, n: String, msg: String): Boolean {
        val latch = CountDownLatch(1)
        acks["$kind:$n"] = latch
        try {
            repeat(ACK_TRIES) {
                send(msg)
                if (latch.await(ACK_WAIT_MS, TimeUnit.MILLISECONDS)) return true
            }
            return false
        } finally {
            acks.remove("$kind:$n")
        }
    }

    private fun nextSmsNo(): Int = smsNo.updateAndGet { if (it >= 255) 1 else it + 1 }

    // ── USSD ─────────────────────────────────────────────

    @Suppress("MissingPermission")
    private fun runUssd(id: String, code: String) {
        val tm = context.getSystemService(TelephonyManager::class.java)
        if (tm == null || code.isEmpty()) {
            send("USSDERROR $id unavailable")
            return
        }
        val ht = ussdThread ?: HandlerThread("goip-ussd").also { it.start(); ussdThread = it }
        listener.onLog("GoIP: USSD $code")
        try {
            tm.sendUssdRequest(code, object : TelephonyManager.UssdResponseCallback() {
                override fun onReceiveUssdResponse(t: TelephonyManager, request: String, response: CharSequence) {
                    send("USSD $id ${clean(response.toString())}")
                }
                override fun onReceiveUssdResponseFailed(t: TelephonyManager, request: String, failureCode: Int) {
                    send("USSDERROR $id failed:$failureCode")
                }
            }, Handler(ht.looper))
        } catch (e: Exception) {
            send("USSDERROR $id ${clean(e.message.orEmpty())}")
        }
    }

    /** No CR/LF inside a protocol line. */
    private fun clean(s: String) = s.replace('\r', ' ').replace('\n', ' ').trim()

    private fun sleepQuiet(ms: Long) {
        try { Thread.sleep(ms) } catch (_: InterruptedException) {}
    }

    companion object {
        private const val TAG = "GoipLink"
        private const val KEEPALIVE_MS = 20_000L
        private const val SESSION_TTL_MS = 10 * 60_000L
        private const val ACK_TRIES = 5
        private const val ACK_WAIT_MS = 3_000L
    }
}
