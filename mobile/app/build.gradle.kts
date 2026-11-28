import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

android {
    namespace = "com.restartfu.offload.mobile"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.restartfu.offload"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
    }

    buildTypes {
        getByName("debug") {
            applicationIdSuffix = ".debug"
        }
        getByName("release") {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"))
        }
    }

    buildFeatures {
        compose = true
    }

    testOptions {
        unitTests.isIncludeAndroidResources = true
        // -Pshots=<dir> makes ScreensTest write its captures there.
        val shots = providers.gradleProperty("shots").orNull
        unitTests.all { test ->
            shots?.let { test.systemProperty("shots", it) }
            test.maxHeapSize = "3g"
            // build.sh: Robolectric keeps its Android jars under user.home/.m2, kept warm in .toolchain.
            // Runners share an address, which Maven Central rate-limits; Google's mirror of it doesn't.
            providers.environmentVariable("OFFLOAD_MOBILE_HOME").orNull?.let {
                test.systemProperty("user.home", it)
                test.systemProperty("robolectric.dependency.repo.url", "https://maven-central.storage-download.googleapis.com/maven2")
            }
            test.testLogging {
                events("passed", "failed", "skipped")
                exceptionFormat = org.gradle.api.tasks.testing.logging.TestExceptionFormat.FULL
            }
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_21
        targetCompatibility = JavaVersion.VERSION_21
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(JvmTarget.JVM_21)
    }
}

dependencies {
    implementation("androidx.activity:activity-compose:1.10.0")
    implementation(platform("androidx.compose:compose-bom:2025.01.01"))
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.ui:ui")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.8.7")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.8.7")
    debugImplementation("androidx.compose.ui:ui-test-manifest")
    testImplementation(platform("androidx.compose:compose-bom:2025.01.01"))
    testImplementation("androidx.compose.ui:ui-test-junit4")
    testImplementation("org.robolectric:robolectric:4.14.1")
    testImplementation("junit:junit:4.13.2")
}
