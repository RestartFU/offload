package com.restartfu.offload.mobile

import java.net.InetAddress
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment

@RunWith(RobolectricTestRunner::class)
class ModelTest {
    @Test fun parsesTheDashboardsFleet() {
        val f = parseFleet(Fixtures.fleet())
        assertEquals(listOf("fsn-build", "hel-build"), f.runners.map { it.name })
        val zeno = f.runners[0]
        assertEquals(22.0, zeno.status!!.capCpu, 0.0)
        assertEquals(24, zeno.metrics!!.cpus.size)
        val w = f.workloads()
        val gh = w.first { it.kind == Workload.Kind.GITHUB && it.active }
        assertEquals("test (ubuntu-latest)", gh.title)
        assertEquals("acme/proxy", gh.repo)
        val build = w.first { it.id == "j-4be1c07a2f" }
        assertEquals("cargo build --release -p bedrock-proxy", build.title)
        assertEquals("proxy-8d2e0f", build.repo)
        assertEquals(5.4, build.use!!.cpu, 0.0)
        assertEquals(101, w.first { it.id == "j-77aa01f3c9" }.exitCode)
        assertNull(build.exitCode)
    }

    @Test fun combinesRunnersOnSharedBuckets() {
        val a = Series(5, listOf(Point(100, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1), Point(105, 3.0, 0.0, 0.0, 0.0, 0.0, 0.0, 1)))
        val b = Series(10, listOf(Point(100, 10.0, 0.0, 0.0, 0.0, 0.0, 0.0, 1)))
        val c = combine(listOf(a, b))!!
        assertEquals(10, c.step)
        assertEquals(1, c.points.size)
        assertEquals(12.0, c.points[0].cpuBusy, 1e-9)
    }

    @Test fun addresses() {
        assertEquals("laptop" to 7421, splitAddress("laptop"))
        assertEquals("laptop" to 8000, splitAddress("http://laptop:8000/"))
        assertEquals("100.64.1.2" to 7421, splitAddress(" 100.64.1.2 "))
        assertEquals("fd7a:115c:a1e0::1" to 7421, splitAddress("[fd7a:115c:a1e0::1]"))
        assertNull(splitAddress("user@laptop"))
        assertNull(splitAddress("laptop/x"))
        assertNull(splitAddress("laptop:99999"))
        assertNull(splitAddress(""))
    }

    @Test fun onlyTheTailnet() {
        assertTrue(onTailnet(InetAddress.getByName("100.64.0.1")))
        assertTrue(onTailnet(InetAddress.getByName("100.127.255.254")))
        assertFalse(onTailnet(InetAddress.getByName("100.128.0.1")))
        assertFalse(onTailnet(InetAddress.getByName("192.168.1.10")))
        assertTrue(onTailnet(InetAddress.getByName("fd7a:115c:a1e0::5")))
        assertFalse(onTailnet(InetAddress.getByName("fd7a:115c:a1e1::5")))
    }

    private fun model(fake: FakeDashboard, store: MemoryStore = MemoryStore(Pairing("laptop", "t0k3n", "laptop"))) = AppModel(
        store, MemoryPrefs(), CoroutineScope(SupervisorJob()), { fake }, { _, _, _ -> error("not pairing") }, { Fixtures.NOW.toEpochMilli() },
    )

    @Test fun aNewerDashboardIsRefusedEveryTime() = runBlocking {
        val fake = FakeDashboard().apply { apiLevel = API_LEVEL + 1 }
        val m = model(fake)
        assertFalse(m.refresh())
        assertFalse("the second try read the fleet anyway", m.refresh())
        assertNull(m.fleet)
        fake.apiLevel = API_LEVEL
        assertTrue(m.refresh())
        assertEquals(3, fake.infoCalls)
    }

    @Test fun forgettingNeedsTheComputersAnswer() = runBlocking {
        val fake = FakeDashboard().apply { failWith = "unreachable" }
        val store = MemoryStore(Pairing("laptop", "t0k3n", "laptop"))
        val m = model(fake, store)
        assertTrue(runCatching { m.unpair() }.isFailure)
        assertEquals("t0k3n", store.p?.token)
        m.unpairHere()
        assertNull(store.p)
        assertNull(m.pairing)
    }

    @Test fun onlyTailscaleCarriesTheToken() {
        fun ip(s: String) = InetAddress.getByName(s)
        val laptop = ip("100.101.102.103")
        val mine = listOf(ip("100.67.150.57"), ip("fd7a:115c:a1e0::39"))
        assertTrue(carriedBy(laptop, mine, listOf(ip("100.64.0.0") to 10)))
        assertTrue(carriedBy(laptop, mine, listOf(ip("100.100.100.100") to 32, ip("100.101.102.103") to 32)))
        assertTrue(carriedBy(ip("fd7a:115c:a1e0::5"), mine, listOf(ip("fd7a:115c:a1e0::") to 48)))
        // Another VPN taking all traffic without a default route: 0.0.0.0/1, or WireGuard's 64.0.0.0/2.
        assertFalse(carriedBy(laptop, mine, listOf(ip("0.0.0.0") to 1, ip("128.0.0.0") to 1)))
        assertFalse(carriedBy(laptop, mine, listOf(ip("64.0.0.0") to 2)))
        assertFalse(carriedBy(laptop, mine, listOf(ip("0.0.0.0") to 0)))
        assertFalse(carriedBy(laptop, mine, listOf(ip("100.64.0.0") to 9)))
        // A route to some other peer only, or a VPN with no tailnet address of its own.
        assertFalse(carriedBy(laptop, mine, listOf(ip("100.99.1.1") to 32)))
        assertFalse(carriedBy(laptop, listOf(ip("10.8.0.2")), listOf(ip("100.64.0.0") to 10)))
        assertFalse(carriedBy(ip("fd7a:115c:a1e0::5"), mine, listOf(ip("fd00::") to 8)))
    }

    @Test fun noVpnNoConnection() {
        val e = runCatching { VpnTailnet(RuntimeEnvironment.getApplication()).open("100.64.0.1", 7421, "/api/info") }.exceptionOrNull()
        assertTrue("$e", e is java.io.IOException && e.message!!.contains("Tailscale is off"))
    }

    @Test fun logStreamSkipsItsHeader() {
        val got = StringBuilder()
        readLogs("{\"offset\":12}\nhello ✓\nworld\n".byteInputStream(), { got.append(it) })
        assertEquals("hello ✓\nworld\n", got.toString())
    }

    @Test fun formats() {
        assertEquals("1.5 GiB", bytes(1.5 * (1L shl 30)))
        assertEquals("12 GiB", bytes(12L shl 30))
        assertEquals("2.5", cores(2.46))
        assertEquals("3", cores(3.0))
        assertEquals("1m 05s", duration(65.0))
        assertEquals("hi", stripAnsi("\u001b[1;32mhi\u001b[0m"))
    }
}
