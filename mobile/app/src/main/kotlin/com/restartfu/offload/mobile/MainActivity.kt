package com.restartfu.offload.mobile

import android.app.Application
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.SystemBarStyle
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.viewModels
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.runtime.LaunchedEffect
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.viewModelScope
import com.restartfu.offload.mobile.ui.OffloadApp
import com.restartfu.offload.mobile.ui.OffloadTheme

class Holder(app: Application) : AndroidViewModel(app) {
    private val prefs = Preferences(app)
    val model = AppModel(
        KeystorePairingStore(app),
        object : AppModel.Prefs {
            override var theme by prefs::theme
            override var range by prefs::range
            override var lastAddress by prefs::lastAddress
        },
        viewModelScope,
    )
}

class MainActivity : ComponentActivity() {
    private val holder: Holder by viewModels()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val model = holder.model
        lifecycle.addObserver(object : DefaultLifecycleObserver {
            override fun onStart(owner: LifecycleOwner) = model.start()
            override fun onStop(owner: LifecycleOwner) = model.stop()
        })
        val name = listOf(Build.MANUFACTURER.replaceFirstChar { it.uppercase() }, Build.MODEL)
            .distinct().joinToString(" ").let { if (Build.MODEL.startsWith(Build.MANUFACTURER, ignoreCase = true)) Build.MODEL else it }
        setContent {
            val light = when (model.theme) {
                ThemeChoice.LIGHT -> true
                ThemeChoice.DARK -> false
                ThemeChoice.SYSTEM -> !isSystemInDarkTheme()
            }
            LaunchedEffect(light) {
                val clear = android.graphics.Color.TRANSPARENT
                val style = if (light) SystemBarStyle.light(clear, clear) else SystemBarStyle.dark(clear)
                enableEdgeToEdge(style, style)
            }
            OffloadTheme(light) { OffloadApp(model, name) }
        }
    }
}
