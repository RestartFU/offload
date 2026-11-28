package com.restartfu.offload.mobile

import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.Color
import android.view.View
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.compose.ui.test.hasScrollToIndexAction
import androidx.compose.ui.test.hasText
import androidx.compose.ui.test.hasContentDescription
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onLast
import androidx.compose.ui.test.performClick
import androidx.compose.ui.test.performScrollToIndex
import androidx.compose.ui.test.performTextReplacement
import androidx.compose.ui.test.hasSetTextAction
import androidx.compose.ui.test.onFirst
import com.restartfu.offload.mobile.ui.OffloadApp
import com.restartfu.offload.mobile.ui.OffloadTheme
import java.io.File
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.runBlocking
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import org.robolectric.annotation.GraphicsMode
import org.robolectric.shadows.ShadowLooper

class MemoryStore(var p: Pairing?) : PairingStore {
    override fun load() = p
    override fun save(p: Pairing) { this.p = p }
    override fun clear() { p = null }
}

class MemoryPrefs(override var theme: ThemeChoice = ThemeChoice.DARK) : AppModel.Prefs {
    override var range = "1h"
    override var lastAddress = ""
}

/**
 * Every page of the app, driven by touch against a scripted dashboard. With `-Pshots=<dir>`
 * it writes each screen there as a PNG.
 */
@RunWith(RobolectricTestRunner::class)
@GraphicsMode(GraphicsMode.Mode.NATIVE)
@Config(sdk = [35], qualifiers = "w411dp-h891dp-420dpi")
class ScreensTest {
    @get:Rule
    val compose = createAndroidComposeRule<ComponentActivity>()

    private val out = System.getProperty("shots")?.let(::File)
    private val paired = Pairing("laptop", "t0k3n", "laptop")

    private fun shoot(
        name: String,
        light: Boolean = false,
        pairing: Pairing? = paired,
        dashboard: FakeDashboard = FakeDashboard(),
        pairer: suspend (String, String, String) -> Pairing = { _, _, _ -> paired },
        before: AppModel.() -> Unit = {},
        steps: Screen.(AppModel) -> Unit = {},
    ): AppModel {
        val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
        val model = AppModel(MemoryStore(pairing), MemoryPrefs(if (light) ThemeChoice.LIGHT else ThemeChoice.DARK), scope, { dashboard }, pairer, { Fixtures.NOW.toEpochMilli() })
        if (pairing != null) {
            runBlocking {
                runCatching { model.refresh() }.onFailure { model.failed(it.message ?: "") }
                runCatching { model.loadSeries() }
                runCatching { model.loadLedger() }
            }
        }
        model.before()
        compose.mainClock.autoAdvance = false
        compose.setContent { OffloadTheme(light) { OffloadApp(model, "Pixel 9", animate = false) } }
        val screen = Screen()
        screen.settle()
        screen.steps(model)
        screen.settle()
        out?.let { dir ->
            dir.mkdirs()
            File(dir, "$name.png").outputStream().use { screen.capture().compress(Bitmap.CompressFormat.PNG, 100, it) }
        }
        scope.cancel()
        return model
    }

    inner class Screen {
        fun settle() {
            repeat(12) {
                Thread.sleep(15)
                compose.mainClock.advanceTimeBy(50)
            }
            ShadowLooper.idleMainLooper()
        }

        fun click(label: String) {
            compose.onAllNodes(hasText(label) or hasContentDescription(label), useUnmergedTree = true).onLast().performClick()
            settle()
        }

        fun type(index: Int, text: String) {
            compose.onAllNodes(hasSetTextAction())[index].performTextReplacement(text)
            settle()
        }

        /**
         * Scrolls the page item by item until [label] is on screen. performScrollToNode animates,
         * which never ends while the test holds the clock.
         */
        fun scrollTo(label: String) {
            val list = compose.onAllNodes(hasScrollToIndexAction()).onFirst()
            val height = list.fetchSemanticsNode().boundsInRoot.bottom
            for (i in 0..60) {
                list.performScrollToIndex(i)
                settle()
                val found = compose.onAllNodes(hasText(label, substring = true), useUnmergedTree = true).fetchSemanticsNodes()
                if (found.any { it.boundsInRoot.top >= 0f && it.boundsInRoot.bottom < height - 260f }) return
            }
            throw AssertionError("\"$label\" never came on screen")
        }

        fun scrollToIndex(i: Int) {
            compose.onAllNodes(hasScrollToIndexAction()).onFirst().performScrollToIndex(i)
            settle()
        }

        fun shows(label: String) = compose.onAllNodes(hasText(label, substring = true), useUnmergedTree = true).fetchSemanticsNodes().isNotEmpty()

        private fun windows(): List<View> {
            val global = Class.forName("android.view.WindowManagerGlobal")
            val instance = global.getMethod("getInstance").invoke(null)
            val field = global.getDeclaredField("mViews").apply { isAccessible = true }
            @Suppress("UNCHECKED_CAST")
            val views = (field.get(instance) as List<View>).toList()
            val screen = compose.activity.window.decorView
            return views.drop(views.indexOf(screen).coerceAtLeast(0))
        }

        /** The activity with any dialogs drawn over it, dimmed as Android dims them. */
        fun capture(): Bitmap {
            ShadowLooper.idleMainLooper()
            val screen = compose.activity.window.decorView
            val bitmap = Bitmap.createBitmap(screen.width, screen.height, Bitmap.Config.ARGB_8888)
            val canvas = Canvas(bitmap)
            screen.draw(canvas)
            for (view in windows()) {
                if (view === screen || view.width == 0) continue
                val dialog = (view.layoutParams as? WindowManager.LayoutParams)?.type == WindowManager.LayoutParams.TYPE_APPLICATION
                if (dialog) canvas.drawColor(Color.argb(150, 0, 0, 0))
                canvas.save()
                canvas.translate((screen.width - view.width) / 2f, (screen.height - view.height) / 2f)
                view.draw(canvas)
                canvas.restore()
            }
            return bitmap
        }
    }

    // ---------- pairing ----------

    @Test fun pairEmpty() { shoot("01-pair", pairing = null) }

    @Test fun pairEmptyLight() { shoot("01-pair-light", light = true, pairing = null) }

    @Test fun pairFilled() {
        shoot("02-pair-filled", pairing = null) {
            type(0, "laptop")
            type(1, "K7QM-4XPD")
        }
    }

    @Test fun pairWrongCode() {
        shoot("03-pair-wrong-code", pairing = null, pairer = { _, _, _ ->
            throw ApiException(403, "wrong pairing code, so it is used up: press Pair a phone in the dashboard again for a new one")
        }) {
            type(0, "laptop")
            type(1, "K7QM-4XPA")
            click("Pair")
            Thread.sleep(200)
            settle()
            check(shows("Wrong pairing code"))
        }
    }

    @Test fun pairThenOverview() {
        val m = shoot("04-paired", pairing = null) {
            type(0, "laptop")
            type(1, "K7QM-4XPD")
            click("Pair")
            Thread.sleep(300)
            settle()
        }
        check(m.pairing == paired)
    }

    @Test fun forgottenByComputer() {
        shoot("05-forgotten", pairing = null, before = { dropPairing("laptop forgot this phone. Pair it again from Devices in the dashboard.") })
    }

    // ---------- overview ----------

    @Test fun overview() { shoot("10-overview") }

    @Test fun overviewLight() { shoot("10-overview-light", light = true) }

    @Test fun overviewCharts() { shoot("11-overview-charts") { scrollToIndex(3) } }

    @Test fun overviewRunners() { shoot("12-overview-runners") { scrollTo("fsn-build") } }

    @Test fun overviewWaiting() { shoot("13-overview-waiting") { scrollTo("not started yet") } }

    @Test fun overviewPaused() {
        shoot("14-overview-paused", dashboard = FakeDashboard({ Fixtures.fleet(paused = true) })) { scrollTo("Paused") }
    }

    @Test fun overviewOffline() {
        shoot("15-overview-runner-offline", dashboard = FakeDashboard({ Fixtures.fleet(offline = true) }))
    }

    @Test fun overviewWeek() { shoot("16-overview-7d") { click("7d"); Thread.sleep(200); settle(); scrollToIndex(3) } }

    @Test fun disconnected() {
        shoot("17-disconnected", dashboard = FakeDashboard(failWith = "Nothing answers at laptop. Is offload dashboard running on the computer?"))
    }

    @Test fun noRunners() {
        shoot("18-no-runners", dashboard = FakeDashboard({ org.json.JSONObject().put("version", "v0.9.3").put("runners", org.json.JSONArray()) }))
    }

    // ---------- containers ----------

    @Test fun containers() { shoot("20-containers") { click("Containers") } }

    @Test fun containersLight() { shoot("20-containers-light", light = true) { click("Containers") } }

    @Test fun containersFinished() { shoot("21-containers-finished") { click("Containers"); click("Finished") } }

    @Test fun containersSessions() { shoot("22-containers-sessions") { click("Containers"); click("Sessions") } }

    @Test fun jobLogs() {
        shoot("23-job-logs") {
            click("Containers")
            click("cargo build --release -p bedrock-proxy")
            click("stderr")
        }
    }

    @Test fun jobLogsLight() {
        shoot("23-job-logs-light", light = true) {
            click("Containers")
            click("cargo build --release -p bedrock-proxy")
            click("stderr")
        }
    }

    @Test fun jobDetails() {
        shoot("24-job-details") {
            click("Containers")
            click("cargo build --release -p bedrock-proxy")
            click("Details")
        }
    }

    @Test fun jobCancel() {
        shoot("25-job-cancel") {
            click("Containers")
            click("cargo build --release -p bedrock-proxy")
            click("Cancel")
        }
    }

    @Test fun finishedJob() {
        shoot("26-job-failed") {
            click("Containers")
            click("Finished")
            click("cargo test -p renderer")
            click("Details")
        }
    }

    @Test fun session() {
        shoot("27-session") {
            click("Containers")
            click("minecraft-bedrock")
        }
    }

    // ---------- GitHub ----------

    @Test fun github() { shoot("30-github") { click("GitHub") } }

    @Test fun githubOwners() { shoot("31-github-owners") { click("GitHub"); scrollTo("Remove acme") } }

    @Test fun githubRemove() { shoot("32-github-remove") { click("GitHub"); scrollTo("Remove acme"); click("Remove acme") } }

    @Test fun githubEmpty() { shoot("33-github-empty", dashboard = FakeDashboard({ Fixtures.fleet(github = false) })) { click("GitHub") } }

    @Test fun githubAdd() {
        shoot("34-github-add") {
            click("GitHub")
            scrollTo("Organizations and repositories")
            click("Add organization")
            type(0, "globex")
        }
    }

    @Test fun githubAddToken() {
        shoot("35-github-add-token") {
            click("GitHub")
            scrollTo("Organizations and repositories")
            click("Add organization")
            type(0, "globex/api")
            click("Use the computer's gh login")
        }
    }

    // ---------- activity ----------

    @Test fun activity() { shoot("40-activity") { click("Activity"); click("24h"); Thread.sleep(200); settle() } }

    @Test fun activityCommands() { shoot("41-activity-commands") { click("Activity"); click("24h"); Thread.sleep(200); settle(); scrollTo("By command") } }

    @Test fun activityLight() { shoot("42-activity-light", light = true) { click("Activity"); click("24h"); Thread.sleep(200); settle() } }

    // ---------- settings ----------

    @Test fun settings() { shoot("50-settings") { click("Settings") } }

    @Test fun settingsLight() { shoot("50-settings-light", light = true) { click("Settings") } }

    @Test fun settingsForget() { shoot("51-settings-forget") { click("Settings"); click("Forget this computer") } }

    @Test fun forgetWhileTheComputerIsAway() {
        val fake = FakeDashboard()
        val m = shoot("53-forget-unreachable", dashboard = fake) {
            click("Settings")
            fake.failWith = "Nothing answers at laptop."
            click("Forget this computer")
            click("Forget")
            Thread.sleep(200)
            settle()
            check(shows("still accepts this phone's key"))
        }
        check(!fake.forgotten && m.pairing != null) { "the phone dropped a key the computer still accepts" }
    }

    @Test fun forgetDropsThePairing() {
        val fake = FakeDashboard()
        val m = shoot("52-forgotten-here", dashboard = fake) {
            click("Settings")
            click("Forget this computer")
            click("Forget")
            Thread.sleep(200)
            settle()
        }
        check(fake.forgotten && m.pairing == null)
    }
}
