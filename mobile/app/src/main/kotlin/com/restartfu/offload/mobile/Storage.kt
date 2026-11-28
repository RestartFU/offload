package com.restartfu.offload.mobile

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec
import org.json.JSONObject

/** Where the pairing lives between launches. */
interface PairingStore {
    fun load(): Pairing?
    fun save(p: Pairing)
    fun clear()
}

/**
 * Keeps the pairing (address, token, the computer's name) as one AES-256-GCM record whose key
 * never leaves Android Keystore. It survives restarts until this device is forgotten.
 */
class KeystorePairingStore(context: Context) : PairingStore {
    private val prefs = context.getSharedPreferences("pairing", Context.MODE_PRIVATE)

    private fun key(): SecretKey {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        (ks.getEntry(ALIAS, null) as? KeyStore.SecretKeyEntry)?.let { return it.secretKey }
        val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore")
        gen.init(
            KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .build(),
        )
        return gen.generateKey()
    }

    override fun load(): Pairing? {
        val blob = prefs.getString("record", null) ?: return null
        return runCatching {
            val raw = Base64.decode(blob, Base64.NO_WRAP)
            val c = Cipher.getInstance("AES/GCM/NoPadding")
            c.init(Cipher.DECRYPT_MODE, key(), GCMParameterSpec(128, raw, 0, 12))
            val o = JSONObject(String(c.doFinal(raw, 12, raw.size - 12)))
            Pairing(o.getString("address"), o.getString("token"), o.optString("name"))
        }.getOrNull()
    }

    override fun save(p: Pairing) {
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.ENCRYPT_MODE, key())
        val body = c.doFinal(JSONObject().put("address", p.address).put("token", p.token).put("name", p.name).toString().toByteArray())
        prefs.edit().putString("record", Base64.encodeToString(c.iv + body, Base64.NO_WRAP)).apply()
    }

    override fun clear() {
        prefs.edit().clear().apply()
    }

    private companion object {
        const val ALIAS = "offload-pairing"
    }
}

enum class ThemeChoice(val key: String, val label: String) {
    SYSTEM("system", "System"),
    DARK("dark", "Dark"),
    LIGHT("light", "Light"),
    ;

    companion object {
        fun from(key: String?) = entries.firstOrNull { it.key == key } ?: SYSTEM
    }
}

/** Small per-phone preferences. */
class Preferences(context: Context) {
    private val prefs = context.getSharedPreferences("settings", Context.MODE_PRIVATE)
    var theme: ThemeChoice
        get() = ThemeChoice.from(prefs.getString("theme", null))
        set(v) = prefs.edit().putString("theme", v.key).apply()
    var range: String
        get() = prefs.getString("range", "1h") ?: "1h"
        set(v) = prefs.edit().putString("range", v).apply()
    var lastAddress: String
        get() = prefs.getString("address", "") ?: ""
        set(v) = prefs.edit().putString("address", v).apply()
}
