import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// Версия — та же, что у бэкенда: build.ps1 передаёт её как -Polcvpn.version=vX.Y.Z.
val olcVersion = (findProperty("olcvpn.version") as String?)?.removePrefix("v") ?: "0.0.0"
val olcCode = olcVersion.split(".").map { it.takeWhile(Char::isDigit).toIntOrNull() ?: 0 }
    .let { (it + listOf(0, 0, 0)).take(3) }
    .let { (maj, min, pat) -> maj * 10000 + min * 100 + pat }
    .coerceAtLeast(1)

// Подпись релиза — из keystore.properties рядом (в git не кладётся):
//   storeFile=..., storePassword=..., keyAlias=..., keyPassword=...
// Без него релиз подписывается отладочным ключом — годится для проверки,
// но обновлять такой APK поверх подписанного настоящим ключом нельзя.
val keystore = Properties().apply {
    val f = rootProject.file("keystore.properties")
    if (f.exists()) f.inputStream().use { load(it) }
}

android {
    namespace = "io.github.marsikcat.olcvpn"
    compileSdk = 36

    defaultConfig {
        applicationId = "io.github.marsikcat.olcvpn"
        minSdk = 26
        targetSdk = 35
        versionCode = olcCode
        versionName = olcVersion
        ndk { abiFilters += listOf("arm64-v8a") }
    }

    signingConfigs {
        if (keystore.isNotEmpty()) {
            create("release") {
                storeFile = rootProject.file(keystore.getProperty("storeFile"))
                storePassword = keystore.getProperty("storePassword")
                keyAlias = keystore.getProperty("keyAlias")
                keyPassword = keystore.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            signingConfig = if (keystore.isNotEmpty()) signingConfigs.getByName("release")
                            else signingConfigs.getByName("debug")
        }
    }

    // Бэкенд, ядро и клиент VK TURN — обычные программы под видом lib*.so:
    // только из каталога нативных библиотек приложению можно их запускать,
    // и для этого они должны лежать распакованными, а не внутри APK.
    packaging {
        jniLibs { useLegacyPackaging = true }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}
